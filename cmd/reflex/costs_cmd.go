package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/kgatilin/reflex/pkg/cost"
)

// newCostsCmd folds llm.usage events out of a JSONL log into a spend report.
// The log is either `--trace-file` output or an audit-handler sink configured
// with `types: [llm.usage]` (the running-agent setup — see examples/agent.yaml).
func newCostsCmd() *cobra.Command {
	var (
		logPath    string
		pricesPath string
		byRequest  bool
	)
	cmd := &cobra.Command{
		Use:   "costs",
		Short: "aggregate llm.usage events from a JSONL log into a cost report",
		Long: "costs reads a JSONL event log (reflex --trace-file output, or an audit\n" +
			"handler sink with types: [llm.usage]), prices the token usage per model,\n" +
			"and prints per-model / per-request / total spend. Default prices ship for\n" +
			"the bootstrap models; override with --prices <json> when they drift.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if logPath == "" {
				return fmt.Errorf("--log is required")
			}
			f, err := os.Open(logPath)
			if err != nil {
				return err
			}
			defer f.Close()

			table := cost.DefaultTable
			if pricesPath != "" {
				pf, err := os.Open(pricesPath)
				if err != nil {
					return err
				}
				defer pf.Close()
				if table, err = cost.LoadTable(pf); err != nil {
					return err
				}
			}

			usages, err := cost.ReadUsageJSONL(f)
			if err != nil {
				return err
			}
			if len(usages) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no llm.usage events in", logPath)
				return nil
			}
			cost.Render(cmd.OutOrStdout(), cost.Aggregate(usages, table), byRequest)
			return nil
		},
	}
	cmd.Flags().StringVar(&logPath, "log", "", "path to the JSONL event log (required)")
	cmd.Flags().StringVar(&pricesPath, "prices", "", "JSON price-table override, merged over defaults")
	cmd.Flags().BoolVar(&byRequest, "by-request", false, "also break the report down per request_id")
	return cmd
}
