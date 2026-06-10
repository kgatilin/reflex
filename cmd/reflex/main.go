// Command reflex runs a single user message through a YAML-configured
// event bus and prints the assistant's response (and optionally the full
// event log).
//
// Usage:
//
//	reflex --config examples/calc.yaml --message "what is 2+2"
//	reflex --config examples/calc.yaml --message "what is 2+2" --trace
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/kgatilin/reflex/internal/runtime"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/handler"
)

func main() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	var (
		configPath string
		message    string
		trace      bool
	)
	cmd := &cobra.Command{
		Use:   "reflex",
		Short: "event-sourcing agent PoC — no loop, just events + YAML-declared subscribers",
		Long: "reflex is a PoC of an agent built as a reactive event bus. " +
			"Subscribers are declared in YAML; they react to events on the log " +
			"and emit new ones; session state is projected, never stored.",
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

			// Summary line so the operator sees what happened.
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
	return cmd
}
