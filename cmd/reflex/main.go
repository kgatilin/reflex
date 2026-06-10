// Command reflex runs a single user message through a YAML-configured
// event bus and prints the assistant's response (and optionally the full
// event log).
//
// Usage:
//
//	reflex run      --config examples/calc.yaml --message "what is 2+2"
//	reflex run      --config examples/calc.yaml --message "what is 2+2" --trace
//	reflex validate --config examples/triage.yaml
//	reflex describe --config examples/triage.yaml
//
// Phase 1.5 introduces validate/describe subcommands; `run` is the explicit
// verb for executing a config. For backwards compatibility, invoking the
// bare reflex binary with --config/--message still runs the bus.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/kgatilin/reflex/internal/runtime"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/graph"
	"github.com/kgatilin/reflex/pkg/handler"
)

func main() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "reflex",
		Short: "event-sourcing agent PoC — no loop, just events + YAML-declared subscribers",
		Long: "reflex is a PoC of an agent built as a reactive event bus. " +
			"Subscribers are declared in YAML; they react to events on the log " +
			"and emit new ones; session state is projected, never stored.",
		SilenceUsage: true,
	}
	root.AddCommand(newRunCmd())
	root.AddCommand(newValidateCmd())
	root.AddCommand(newDescribeCmd())
	return root
}

func newRunCmd() *cobra.Command {
	var (
		configPath string
		message    string
		trace      bool
		traceFile  string
	)
	cmd := &cobra.Command{
		Use:          "run",
		Short:        "run a single user message through the configured bus",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if configPath == "" {
				return fmt.Errorf("--config is required")
			}
			if message == "" {
				return fmt.Errorf("--message is required")
			}

			reg := handler.BuiltinRegistry()
			cfg, err := config.Load(configPath, reg.Types())
			if err != nil {
				return err
			}
			b, err := runtime.Build(cfg, reg)
			if err != nil {
				return err
			}
			res, err := runtime.Run(context.Background(), b, message)
			if err != nil {
				return err
			}

			if trace {
				enc := json.NewEncoder(cmd.OutOrStdout())
				for _, e := range res.Events {
					if err := enc.Encode(e); err != nil {
						return err
					}
				}
			}

			// --trace-file writes the full event log as one JSON object per
			// line to a file, leaving stdout free for the printer handler's
			// human-readable output. This is the clean-machine-readable
			// counterpart to --trace (which mixes JSONL into stdout). The
			// two flags are independent — set either, both, or neither.
			if traceFile != "" {
				f, ferr := os.Create(traceFile)
				if ferr != nil {
					return fmt.Errorf("--trace-file: %w", ferr)
				}
				enc := json.NewEncoder(f)
				for _, e := range res.Events {
					if err := enc.Encode(e); err != nil {
						_ = f.Close()
						return err
					}
				}
				if cerr := f.Close(); cerr != nil {
					return fmt.Errorf("--trace-file close: %w", cerr)
				}
			}

			if res.State.Unhandled {
				fmt.Fprintf(cmd.OutOrStderr(),
					"request %s unhandled: %s\n",
					res.RequestID, res.State.UnhandledReason)
				os.Exit(2)
			} else if !res.State.Handled {
				fmt.Fprintf(cmd.OutOrStderr(),
					"request %s did not produce RequestHandled (and no watcher)\n",
					res.RequestID)
				os.Exit(2)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "path to handler YAML")
	cmd.Flags().StringVar(&message, "message", "", "user message to feed into RequestReceived")
	cmd.Flags().BoolVar(&trace, "trace", false, "dump the full event log to stdout (one JSON object per line)")
	cmd.Flags().StringVar(&traceFile, "trace-file", "", "write the full event log to a file as one JSON object per line (does not mix with stdout)")
	return cmd
}

func newValidateCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:          "validate",
		Short:        "compile the YAML config into a handler graph and check for uncapped cycles",
		Long:         "validate exits 0 when the config compiles to a cycle-free graph (or one where every cycle has an explicit max_iterations cap), and exits 1 otherwise. The error message names the offending cycle(s).",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if configPath == "" {
				return fmt.Errorf("--config is required")
			}
			reg := handler.BuiltinRegistry()
			cfg, err := config.Load(configPath, reg.Types())
			if err != nil {
				return err
			}
			g, err := graph.Build(cfg, reg)
			if err != nil {
				var ce *graph.CycleError
				if errors.As(err, &ce) {
					fmt.Fprintln(cmd.OutOrStderr(), err.Error())
					os.Exit(1)
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"config valid: %d handlers, %d edges, %d declared loops\n",
				len(g.Nodes), len(g.Edges), len(g.DeclaredLoops))
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "path to handler YAML")
	return cmd
}

func newDescribeCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:          "describe",
		Short:        "print the handler graph as a textual table (name, type, description, consumes, emits)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if configPath == "" {
				return fmt.Errorf("--config is required")
			}
			reg := handler.BuiltinRegistry()
			cfg, err := config.Load(configPath, reg.Types())
			if err != nil {
				return err
			}
			// describe must work even when the config is cyclic: humans
			// inspect a broken topology more often than a healthy one.
			g, err := graph.Build(cfg, reg)
			if g == nil {
				return err
			}
			if err := g.Describe(cmd.OutOrStdout()); err != nil {
				return err
			}
			if err != nil {
				fmt.Fprintln(cmd.OutOrStderr(), "warning:", err.Error())
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "path to handler YAML")
	return cmd
}
