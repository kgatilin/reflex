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
	"time"

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
			// Phase 4b: install the post-drain quiescence check so the
			// daemon surfaces RequestUnhandled / EventOrphaned diagnostics
			// the same way `reflex run` does.
			d.SetQuiescence(handler.CheckQuiescence)

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

// emitToDaemon connects to a daemon over the given socket, sends a single
// seed event, optionally registers a wait predicate, and disconnects when
// the predicate resolves (or immediately when no predicate is set).
//
// Phase 4b: when `wait` is non-empty the CLI installs an Await frame on
// the daemon side; the daemon evaluates the predicate after every drain
// and replies with Resolved (or Timeout). The CLI blocks on the inbound
// channel; the daemon does NOT block its drain — multiple in-flight
// predicates are evaluated lazily after each drain step.
func emitToDaemon(ctx context.Context, socketPath, eventType string, payload map[string]any, wait string) error {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return fmt.Errorf("connect %s: %w", socketPath, err)
	}
	defer conn.Close()

	// Hello with a dummy handler that consumes a never-fired event type so
	// it never gets delivered to.
	helloSpec := sdk.HandlerSpec{
		Name:     "_cli_seed",
		Consumes: "__noop__",
	}
	enc := newLineEncoder(conn)
	if err := enc.encode(sdk.Frame{Kind: sdk.KindHello, Version: sdk.ProtocolVersion, Handler: &helloSpec}); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	dec := newLineDecoder(conn)
	wf, err := dec.decode()
	if err != nil {
		return fmt.Errorf("welcome: %w", err)
	}
	if wf.Kind != sdk.KindWelcome {
		return fmt.Errorf("expected welcome, got %q: %s", wf.Kind, wf.Error)
	}

	// Build the seed.
	seed := buildSeedEvent(eventType, payload)

	// If we have a wait predicate, install it BEFORE emit so the daemon's
	// post-drain await check sees the resolved condition.
	if wait != "" {
		awaitID := "cli-await-1"
		if err := enc.encode(sdk.Frame{
			Kind:      sdk.KindAwait,
			AwaitID:   awaitID,
			Predicate: wait,
			RequestID: seed.RequestID,
		}); err != nil {
			return fmt.Errorf("await: %w", err)
		}
	}

	if err := enc.encode(sdk.Frame{Kind: sdk.KindEmit, Event: &seed}); err != nil {
		return fmt.Errorf("emit: %w", err)
	}

	// If no wait predicate: send goodbye and return. The daemon owns the
	// drain.
	if wait == "" {
		_ = enc.encode(sdk.Frame{Kind: sdk.KindGoodbye})
		return nil
	}

	// Wait for the daemon to resolve the predicate. Deadline keeps the CLI
	// from hanging forever on a misbehaving daemon.
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()

	type frameOrErr struct {
		f   sdk.Frame
		err error
	}
	frames := make(chan frameOrErr, 1)
	go func() {
		for {
			f, err := dec.decode()
			if err != nil {
				frames <- frameOrErr{err: err}
				return
			}
			frames <- frameOrErr{f: f}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("await %q: timeout after 30s", wait)
		case fe := <-frames:
			if fe.err != nil {
				return fmt.Errorf("await read: %w", fe.err)
			}
			switch fe.f.Kind {
			case sdk.KindResolved:
				_ = enc.encode(sdk.Frame{Kind: sdk.KindGoodbye})
				return nil
			case sdk.KindError:
				return fmt.Errorf("daemon: %s", fe.f.Error)
			}
		}
	}
}
