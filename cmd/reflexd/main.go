// Command reflexd is the new-kernel daemon and its client CLI (doc 29 Iteration
// 2 / CONCEPT §8): `serve` hosts an engine behind a unix-socket API, and the
// other subcommands are clients of that API — apply a topology, emit a message,
// read the live table or the log. `validate` is a local dry-run needing no
// daemon. This is the runtime shell around the new engine; the legacy
// cmd/reflex is frozen and untouched.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/kgatilin/reflex/engine"
	"github.com/kgatilin/reflex/pkg/daemon"
	"github.com/kgatilin/reflex/pkg/topology"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func main() {
	if err := root().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func root() *cobra.Command {
	var socket string
	cmd := &cobra.Command{
		Use:           "reflexd",
		Short:         "reflex daemon + control-plane CLI (new kernel)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.PersistentFlags().StringVar(&socket, "socket", "/tmp/reflexd.sock", "daemon unix socket path")
	cmd.AddCommand(serveCmd(&socket), applyCmd(&socket), topologyCmd(&socket), emitCmd(&socket), eventsCmd(&socket), validateCmd(), pluginCmd())
	return cmd
}

// serveCmd hosts the daemon until interrupted. --root, when set, launches the
// out-of-process hands (fs, pytest) confined to that workspace root: the root is
// a host concern — reflexd determines it and passes it at plugin init (`reflexd
// plugin fs --root <dir>`), it is never an operator topology config. Each plugin
// self-registers its handler + catalog kinds on connect; the operator then
// applies a topology that emits the kinds those hands consume.
func serveCmd(socket *string) *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "host the engine behind the unix-socket API",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			d := daemon.New()
			defer d.Close()
			if root != "" {
				self, err := os.Executable()
				if err != nil {
					return fmt.Errorf("locating the reflexd binary to launch plugins: %w", err)
				}
				for _, name := range []string{"fs", "pytest"} {
					if _, err := d.LaunchPlugin(ctx, []string{self, "plugin", name, "--root", root}); err != nil {
						return fmt.Errorf("launching %q plugin: %w", name, err)
					}
				}
				fmt.Fprintf(cmd.OutOrStdout(), "launched fs, pytest plugins (root %s)\n", root)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "reflexd serving on %s\n", *socket)
			return daemon.Serve(ctx, d, *socket)
		},
	}
	cmd.Flags().StringVar(&root, "root", "", "workspace root; when set, launch the fs and pytest plugins confined to it")
	return cmd
}

// applyCmd sends a topology document (YAML/JSON file) to the daemon.
func applyCmd(socket *string) *cobra.Command {
	return &cobra.Command{
		Use:   "apply FILE",
		Short: "apply a topology document to the running daemon",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			doc, err := readDocFile(args[0])
			if err != nil {
				return err
			}
			res, err := daemon.Dial(*socket).Apply(cmd.Context(), doc)
			if err != nil {
				if res.Report != nil {
					printReport(cmd.OutOrStdout(), *res.Report)
				}
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "applied")
			return nil
		},
	}
}

// topologyCmd prints the daemon's live table as YAML.
func topologyCmd(socket *string) *cobra.Command {
	return &cobra.Command{
		Use:   "topology",
		Short: "print the daemon's live topology",
		RunE: func(cmd *cobra.Command, _ []string) error {
			doc, err := daemon.Dial(*socket).Topology(cmd.Context())
			if err != nil {
				return err
			}
			return yaml.NewEncoder(cmd.OutOrStdout()).Encode(doc)
		},
	}
}

// emitCmd sends one ingress event, drains, and prints the produced event kinds.
// --wait names a kind that must appear in the reconciliation, else it errors.
func emitCmd(socket *string) *cobra.Command {
	var subject, payload, wait string
	var noDrain bool
	cmd := &cobra.Command{
		Use:   "emit",
		Short: "append an ingress event and drive the engine",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if subject == "" {
				return fmt.Errorf("--subject is required")
			}
			events, err := daemon.Dial(*socket).Emit(cmd.Context(), subject, json.RawMessage(payload), !noDrain)
			if err != nil {
				return err
			}
			seen := false
			for _, ev := range events {
				k := engine.KindOf(ev)
				fmt.Fprintln(cmd.OutOrStdout(), k)
				if wait != "" && k == wait {
					seen = true
				}
			}
			if wait != "" && !seen {
				return fmt.Errorf("waited for kind %q but it did not occur in %d produced events", wait, len(events))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&subject, "subject", "", "ingress subject, e.g. app.ingress.cli.task")
	cmd.Flags().StringVar(&payload, "payload", "{}", "ingress payload JSON")
	cmd.Flags().StringVar(&wait, "wait", "", "require this kind to appear in the reconciliation")
	cmd.Flags().BoolVar(&noDrain, "no-drain", false, "append without draining to quiescence")
	return cmd
}

// eventsCmd prints the daemon's whole log as JSON.
func eventsCmd(socket *string) *cobra.Command {
	return &cobra.Command{
		Use:   "events",
		Short: "print the daemon's event log",
		RunE: func(cmd *cobra.Command, _ []string) error {
			events, err := daemon.Dial(*socket).Events(cmd.Context())
			if err != nil {
				return err
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(events)
		},
	}
}

// validateCmd dry-runs a topology document locally (no daemon): it validates the
// file as a standalone topology and reports the gaps.
func validateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate FILE",
		Short: "validate a topology document locally (no daemon)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			doc, err := readDocFile(args[0])
			if err != nil {
				return err
			}
			decls, err := doc.Decls()
			if err != nil {
				return err
			}
			rep, err := engine.Validate(decls...)
			if err != nil {
				return err
			}
			printReport(cmd.OutOrStdout(), rep)
			if !rep.Connected {
				return fmt.Errorf("topology is not connected")
			}
			return nil
		},
	}
}

func readDocFile(path string) (topology.Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return topology.Document{}, err
	}
	return topology.Parse(data)
}

func printReport(w interface{ Write([]byte) (int, error) }, rep engine.Report) {
	if rep.Connected {
		fmt.Fprintln(w, "connected: ok")
		return
	}
	fmt.Fprintln(w, "connected: NO")
	for _, s := range rep.Suggestions {
		fmt.Fprintln(w, "  -", s)
	}
}
