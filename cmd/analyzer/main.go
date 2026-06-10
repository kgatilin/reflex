// Command reflex-analyzer is the Phase 3 analyzer engine for reflex.
//
// Usage:
//
//	reflex-analyzer --trace events.jsonl
//	reflex-analyzer --trace events.jsonl --json
//	reflex-analyzer --trace events.jsonl --request-id <uuid>
//	reflex-analyzer --watch ./traces/
//
// The analyzer consumes a JSON-Lines event log produced by
// `reflex run --trace-file events.jsonl` (or `reflex run --trace` piped
// through a JSON filter — the reader tolerates the printer's mixed
// human/JSON output too). It computes causal-DAG metrics + objective.
//
// See pkg/analyzer for the metric definitions.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/kgatilin/reflex/pkg/analyzer"
)

func main() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	var (
		tracePath string
		watchDir  string
		reqID     string
		metric    string
		jsonOut   bool
	)
	cmd := &cobra.Command{
		Use:          "reflex-analyzer",
		Short:        "background analyzer engine for reflex event traces",
		Long:         "reflex-analyzer reads a JSON-Lines reflex event trace (produced by `reflex run --trace-file`), builds a causal DAG, and computes width/depth/orphan/termination metrics plus a single objective scalar the optimisation loop will minimise.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch {
			case watchDir != "":
				ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
				defer cancel()
				w := analyzer.NewWatcher(watchDir, jsonOut, cmd.OutOrStdout())
				if err := w.Run(ctx); err != nil && err != context.Canceled {
					return err
				}
				return nil
			case tracePath != "":
				tr, err := analyzer.ReadTraceFile(tracePath)
				if err != nil {
					return err
				}
				rep, err := analyzer.Analyze(tr)
				if err != nil {
					return err
				}
				rep = rep.FilterRequest(reqID)
				if jsonOut {
					return rep.PrintJSON(cmd.OutOrStdout())
				}
				if metric != "" {
					return printSingleMetric(cmd.OutOrStdout(), rep, metric, reqID)
				}
				rep.PrintText(cmd.OutOrStdout())
				return nil
			default:
				return fmt.Errorf("either --trace <file> or --watch <dir> is required")
			}
		},
	}
	cmd.Flags().StringVar(&tracePath, "trace", "", "path to a JSONL event trace produced by reflex run --trace-file")
	cmd.Flags().StringVar(&watchDir, "watch", "", "watch a directory of *.jsonl traces and re-analyze on change")
	cmd.Flags().StringVar(&reqID, "request-id", "", "narrow the report to a single request_id (UUID)")
	cmd.Flags().StringVar(&metric, "metric", "", "print a single metric value (one of: causal_depth, causal_width, orphans, objective)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the full report as indented JSON instead of the text summary")
	return cmd
}

// printSingleMetric writes one metric's value as plain text. Used for
// shell-pipeline-friendly output: `analyzer --trace ... --metric objective`
// prints exactly one number.
func printSingleMetric(w io.Writer, rep *analyzer.Report, name, reqID string) error {
	switch name {
	case "objective":
		fmt.Fprintln(w, rep.Objective)
		return nil
	case "causal_depth":
		if reqID != "" {
			if rm, ok := rep.Metrics.PerRequest[reqID]; ok {
				fmt.Fprintln(w, rm.CausalDepth)
				return nil
			}
			return fmt.Errorf("request-id %s not found", reqID)
		}
		best := 0
		for _, rm := range rep.Metrics.PerRequest {
			if rm.CausalDepth > best {
				best = rm.CausalDepth
			}
		}
		fmt.Fprintln(w, best)
		return nil
	case "causal_width":
		if reqID != "" {
			if rm, ok := rep.Metrics.PerRequest[reqID]; ok {
				fmt.Fprintln(w, rm.CausalWidth)
				return nil
			}
			return fmt.Errorf("request-id %s not found", reqID)
		}
		best := 0
		for _, rm := range rep.Metrics.PerRequest {
			if rm.CausalWidth > best {
				best = rm.CausalWidth
			}
		}
		fmt.Fprintln(w, best)
		return nil
	case "orphans":
		fmt.Fprintln(w, len(rep.Metrics.Orphans))
		return nil
	}
	return fmt.Errorf("unknown metric %q (expected: causal_depth, causal_width, orphans, objective)", name)
}
