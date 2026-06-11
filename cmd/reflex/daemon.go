package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/kgatilin/reflex/internal/runtime"
	"github.com/kgatilin/reflex/pkg/handler"
	"github.com/kgatilin/reflex/pkg/sdk"
)

// defaultDaemonSocket returns the default Unix-socket path:
// ${XDG_RUNTIME_DIR}/reflex.sock, falling back to /tmp/reflex.sock when
// XDG_RUNTIME_DIR is unset (CI, containers, macOS).
func defaultDaemonSocket() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "reflex.sock")
	}
	return "/tmp/reflex.sock"
}

func newDaemonCmd() *cobra.Command {
	var (
		configPath string
		socketPath string
	)
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "run reflex as a long-running bus process accepting handler connections over a Unix socket",
		Long: `daemon loads the YAML config the same way 'reflex run' does and stays alive,
listening on a Unix socket for handler clients to connect, register, and react
to events. CLI clients (e.g. 'reflex emit --daemon ...') can also dial in to
seed events.

Graceful shutdown on SIGINT/SIGTERM: drain finishes the current request, the
socket closes, the process exits 0.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if configPath == "" {
				return fmt.Errorf("--config is required")
			}
			if socketPath == "" {
				socketPath = defaultDaemonSocket()
			}

			cfg, err := loadConfig(configPath)
			if err != nil {
				return err
			}
			reg := handler.BuiltinRegistry()
			b, err := runtime.Build(cfg, reg)
			if err != nil {
				return err
			}

			// Best-effort: remove a stale socket left behind by a previous
			// process so we don't fail on EADDRINUSE.
			if _, err := os.Stat(socketPath); err == nil {
				if rmErr := os.Remove(socketPath); rmErr != nil {
					return fmt.Errorf("daemon: remove stale socket %s: %w", socketPath, rmErr)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("daemon: stat socket %s: %w", socketPath, err)
			}

			lis, err := net.Listen("unix", socketPath)
			if err != nil {
				return fmt.Errorf("daemon: listen %s: %w", socketPath, err)
			}
			// Tighten permissions: socket should only be reachable by the
			// owner. Best-effort; ignore failure.
			_ = os.Chmod(socketPath, 0o600)

			d := sdk.NewDaemon(b, lis)

			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			fmt.Fprintf(cmd.OutOrStdout(), "reflex daemon: listening on %s (config: %s)\n", socketPath, configPath)
			if err := d.Serve(ctx); err != nil {
				return fmt.Errorf("daemon: serve: %w", err)
			}
			// Tidy up the socket file on clean exit.
			_ = os.Remove(socketPath)
			fmt.Fprintln(cmd.OutOrStdout(), "reflex daemon: stopped")
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "path to handler YAML")
	cmd.Flags().StringVar(&socketPath, "socket", "", "Unix socket path (default: $XDG_RUNTIME_DIR/reflex.sock or /tmp/reflex.sock)")
	return cmd
}

// emitToDaemon connects to a daemon over the given socket and sends a
// single bare emit frame, then waits for either a daemon-side error or a
// clean EOF (the daemon does not currently send any post-drain signal back
// — see TODO below). Used by 'reflex emit --daemon ...'.
//
// Returns the err the daemon reported, or nil on clean disconnect.
// TODO(phase-4b): the daemon should ACK the seed with a drain summary so
// CLI wait-predicates can be evaluated here instead of just locally on the
// post-drain state.
func emitToDaemon(ctx context.Context, socketPath, eventType string, payload map[string]any) error {
	// Re-use the SDK Connect path for the dial — but we don't want to
	// register a handler. Drop down to a direct net.Dial.
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return fmt.Errorf("connect %s: %w", socketPath, err)
	}
	defer conn.Close()

	// We still need to handshake — the daemon's handleConn expects a
	// hello frame. Send a dummy handler that consumes a never-fired event
	// type so it never gets delivered to.
	helloSpec := sdk.HandlerSpec{
		Name:     "_cli_seed",
		Consumes: "__noop__",
	}
	enc := newLineEncoder(conn)
	if err := enc.encode(sdk.Frame{Kind: sdk.KindHello, Version: sdk.ProtocolVersion, Handler: &helloSpec}); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	// Read welcome.
	dec := newLineDecoder(conn)
	wf, err := dec.decode()
	if err != nil {
		return fmt.Errorf("welcome: %w", err)
	}
	if wf.Kind != sdk.KindWelcome {
		return fmt.Errorf("expected welcome, got %q: %s", wf.Kind, wf.Error)
	}

	// Emit the seed.
	seed := buildSeedEvent(eventType, payload)
	if err := enc.encode(sdk.Frame{Kind: sdk.KindEmit, Event: &seed}); err != nil {
		return fmt.Errorf("emit: %w", err)
	}
	// Goodbye and let the daemon close.
	_ = enc.encode(sdk.Frame{Kind: sdk.KindGoodbye})
	return nil
}
