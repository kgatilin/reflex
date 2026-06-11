// Command reflex runs a single user message through a YAML-configured
// event bus and prints the assistant's response (and optionally the full
// event log).
//
// Usage:
//
//	reflex run      --config examples/calc.yaml --message "what is 2+2"
//	reflex run      --config examples/calc.yaml --message "what is 2+2" --trace
//	reflex emit     --config examples/aggregate.yaml --type ClassifyRequested --payload '{"item":"foo"}'
//	reflex invoke   --config examples/triage.yaml triage archai#114
//	reflex send     --config examples/calc.yaml "what is 2+2"
//	reflex validate --config examples/triage.yaml
//	reflex describe --config examples/triage.yaml
//
// Phase 1.6 adds:
//   - emit / invoke / send subcommands for direct event emission.
//   - --wait predicates: drain | request_id_terminal | projection.has=<key>.
//   - YAML events: section declaring per-event CLI bindings + default wait.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/kgatilin/reflex/internal/runtime"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/graph"
	"github.com/kgatilin/reflex/pkg/handler"
	"github.com/kgatilin/reflex/pkg/projection"
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
	root.AddCommand(newEmitCmd())
	root.AddCommand(newInvokeCmd())
	root.AddCommand(newSendCmd())
	root.AddCommand(newValidateCmd())
	root.AddCommand(newDescribeCmd())
	root.AddCommand(newDaemonCmd())
	root.AddCommand(newNewHandlerCmd())
	root.AddCommand(newAnalyzeCmd())
	return root
}

// runOpts is the shared option-set for the run / emit / invoke / send
// commands — they all assemble a bus, seed an event, drain, and surface
// the trace.
type runOpts struct {
	configPath string
	trace      bool
	traceFile  string
	wait       string
	daemon     string // when set, emit to a running daemon over this Unix socket
}

func addRunFlags(cmd *cobra.Command, o *runOpts) {
	cmd.Flags().StringVar(&o.configPath, "config", "", "path to handler YAML")
	cmd.Flags().BoolVar(&o.trace, "trace", false, "dump the full event log to stdout (one JSON object per line)")
	cmd.Flags().StringVar(&o.traceFile, "trace-file", "", "write the full event log to a file as one JSON object per line (does not mix with stdout)")
	cmd.Flags().StringVar(&o.wait, "wait", "", "wait-predicate after emission: drain | request_id_terminal | projection.has=<key>")
	cmd.Flags().StringVar(&o.daemon, "daemon", "", "send the seed event to a running daemon over this Unix socket instead of running in-process")
}

func newRunCmd() *cobra.Command {
	var (
		opts    runOpts
		message string
	)
	cmd := &cobra.Command{
		Use:          "run",
		Short:        "run a single user message through the configured bus",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if opts.configPath == "" {
				return fmt.Errorf("--config is required")
			}
			if message == "" {
				return fmt.Errorf("--message is required")
			}
			res, err := executeRun(cmd.Context(), opts.configPath, "RequestReceived",
				map[string]any{"payload": message})
			if err != nil {
				return err
			}
			return finalise(cmd, res, &opts)
		},
	}
	addRunFlags(cmd, &opts)
	cmd.Flags().StringVar(&message, "message", "", "user message to feed into RequestReceived")
	return cmd
}

// newEmitCmd is the lowest-level entry point: emit any event type with a
// JSON payload, then drain. Used to test handler chains in isolation and
// as the substrate the invoke / send sugar dispatches through.
func newEmitCmd() *cobra.Command {
	var (
		opts       runOpts
		eventType  string
		payloadStr string
	)
	cmd := &cobra.Command{
		Use:          "emit",
		Short:        "emit a single event into the bus and drain",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if opts.configPath == "" && opts.daemon == "" {
				return fmt.Errorf("--config or --daemon is required")
			}
			if eventType == "" {
				return fmt.Errorf("--type is required")
			}
			payload := map[string]any{}
			if payloadStr != "" {
				if err := json.Unmarshal([]byte(payloadStr), &payload); err != nil {
					return fmt.Errorf("--payload: %w", err)
				}
			}
			if opts.daemon != "" {
				// Daemon owns config / drain / trace; CLI just ships the
				// seed. Local wait-predicates can't observe daemon state
				// in Phase 4a — see TODO in emitToDaemon.
				return emitToDaemon(cmd.Context(), opts.daemon, eventType, payload, opts.wait)
			}
			// Apply event-config defaults (wait predicate) if the YAML
			// declares this type.
			cfg, err := loadConfig(opts.configPath)
			if err != nil {
				return err
			}
			if opts.wait == "" {
				if ec := findEventConfig(cfg, eventType); ec != nil && ec.CLI != nil && ec.CLI.Wait != "" {
					opts.wait = ec.CLI.Wait
				}
			}
			res, err := executeRunWithConfig(cmd.Context(), cfg, eventType, payload)
			if err != nil {
				return err
			}
			return finalise(cmd, res, &opts)
		},
	}
	addRunFlags(cmd, &opts)
	cmd.Flags().StringVar(&eventType, "type", "", "event type to emit (e.g. RequestReceived)")
	cmd.Flags().StringVar(&payloadStr, "payload", "", "event payload as a JSON object")
	return cmd
}

// newInvokeCmd looks up an event by its declared CLI command and emits it.
// Positional args after the command name become the value for the first
// arg key declared on the event. This is intentionally minimal — Phase 1.6
// is a usability layer, not a full type system.
func newInvokeCmd() *cobra.Command {
	var opts runOpts
	cmd := &cobra.Command{
		Use:          "invoke <command> [arg]",
		Short:        "invoke a YAML-declared event command (e.g. `invoke triage archai#114`)",
		SilenceUsage: true,
		Args:         cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.configPath == "" {
				return fmt.Errorf("--config is required")
			}
			cfg, err := loadConfig(opts.configPath)
			if err != nil {
				return err
			}
			command := strings.Join(args, " ")
			ec, eventType := findEventByCommand(cfg, "invoke "+command)
			if ec == nil {
				// fall back: try matching just the first arg as command name
				ec, eventType = findEventByCommand(cfg, "invoke "+args[0])
				if ec == nil {
					return fmt.Errorf("no event binding for command %q", command)
				}
				args = args[1:]
			} else {
				args = nil
			}
			payload := buildPayload(ec, args)
			if opts.wait == "" && ec.CLI != nil {
				opts.wait = ec.CLI.Wait
			}
			if opts.daemon != "" {
				return emitToDaemon(cmd.Context(), opts.daemon, eventType, payload, opts.wait)
			}
			res, err := executeRunWithConfig(cmd.Context(), cfg, eventType, payload)
			if err != nil {
				return err
			}
			return finalise(cmd, res, &opts)
		},
	}
	addRunFlags(cmd, &opts)
	return cmd
}

// newSendCmd is the user-facing "send a message" sugar: looks for the
// event whose CLI command starts with "send" and emits it with the
// message as the first arg.
func newSendCmd() *cobra.Command {
	var opts runOpts
	cmd := &cobra.Command{
		Use:          "send <text>",
		Short:        "shorthand for emitting the event bound to `cli.command: send ...` with the given text",
		SilenceUsage: true,
		Args:         cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.configPath == "" {
				return fmt.Errorf("--config is required")
			}
			cfg, err := loadConfig(opts.configPath)
			if err != nil {
				return err
			}
			text := strings.Join(args, " ")
			ec, eventType := findEventByCommand(cfg, "send")
			if ec == nil {
				return fmt.Errorf("no event binding for command \"send\"")
			}
			payload := buildPayload(ec, []string{text})
			if opts.wait == "" && ec.CLI != nil {
				opts.wait = ec.CLI.Wait
			}
			if opts.daemon != "" {
				return emitToDaemon(cmd.Context(), opts.daemon, eventType, payload, opts.wait)
			}
			res, err := executeRunWithConfig(cmd.Context(), cfg, eventType, payload)
			if err != nil {
				return err
			}
			return finalise(cmd, res, &opts)
		},
	}
	addRunFlags(cmd, &opts)
	return cmd
}

// executeRun is the original `reflex run` flow generalised: build a bus
// from configPath, emit a seed event of the given type + payload, drain,
// return the Result.
func executeRun(ctx context.Context, configPath, eventType string, payload map[string]any) (*runtime.Result, error) {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return nil, err
	}
	return executeRunWithConfig(ctx, cfg, eventType, payload)
}

func executeRunWithConfig(ctx context.Context, cfg *config.File, eventType string, payload map[string]any) (*runtime.Result, error) {
	reg := handler.BuiltinRegistry()
	b, err := runtime.Build(cfg, reg)
	if err != nil {
		return nil, err
	}
	reqID := uuid.NewString()
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	seed := event.Event{
		Type:      eventType,
		RequestID: reqID,
		Source:    "cli",
		Payload:   raw,
	}
	if err := b.Run(ctx, seed); err != nil {
		return nil, err
	}
	if err := handler.CheckQuiescence(ctx, b); err != nil {
		return nil, err
	}
	all := b.Store().Snapshot()
	state := projection.SessionProjection(all, reqID)
	return &runtime.Result{
		RequestID:  reqID,
		Events:     all,
		State:      state,
		Projection: b.Projection(),
	}, nil
}

func loadConfig(path string) (*config.File, error) {
	reg := handler.BuiltinRegistry()
	return config.Load(path, reg.Types())
}

func findEventConfig(cfg *config.File, name string) *config.EventConfig {
	for i := range cfg.Events {
		if cfg.Events[i].Name == name {
			return &cfg.Events[i]
		}
	}
	return nil
}

// findEventByCommand looks up an event whose cli.command exactly matches
// the given string. Returns (eventConfig, name).
func findEventByCommand(cfg *config.File, command string) (*config.EventConfig, string) {
	command = strings.TrimSpace(command)
	for i := range cfg.Events {
		ec := &cfg.Events[i]
		if ec.CLI == nil {
			continue
		}
		if strings.TrimSpace(ec.CLI.Command) == command {
			return ec, ec.Name
		}
	}
	return nil, ""
}

// buildPayload turns positional args into a payload map by binding them
// to the keys declared in ec.Args in order. If there are no declared args
// and one positional, default the key to "payload" (legacy behaviour).
func buildPayload(ec *config.EventConfig, args []string) map[string]any {
	out := map[string]any{}
	if len(ec.Args) == 0 {
		if len(args) > 0 {
			out["payload"] = strings.Join(args, " ")
		}
		return out
	}
	keys := orderedArgKeys(ec)
	for i, key := range keys {
		if i >= len(args) {
			break
		}
		out[key] = args[i]
	}
	// If exactly one arg key was declared and only one positional, also
	// expose as "payload" for handler backward-compatibility.
	if len(keys) == 1 && len(args) == 1 {
		out["payload"] = args[0]
	}
	return out
}

// orderedArgKeys returns the arg keys for an EventConfig in YAML
// declaration order. Because Args is a map[string]any, we read order from
// any embedded list-style declaration when possible; otherwise we sort
// keys for determinism.
func orderedArgKeys(ec *config.EventConfig) []string {
	out := make([]string, 0, len(ec.Args))
	for k := range ec.Args {
		out = append(out, k)
	}
	// Deterministic order — alphabetical. YAML's map iteration order is
	// random in Go, so sorting is the only stable option without a full
	// re-parse.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// finalise handles --trace / --trace-file / --wait, then prints the
// unhandled-request diagnostic and exits with code 2 if needed.
func finalise(cmd *cobra.Command, res *runtime.Result, opts *runOpts) error {
	if opts.trace {
		enc := json.NewEncoder(cmd.OutOrStdout())
		for _, e := range res.Events {
			if err := enc.Encode(e); err != nil {
				return err
			}
		}
	}
	if opts.traceFile != "" {
		f, ferr := os.Create(opts.traceFile)
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
	if opts.wait != "" {
		ok, why := checkWaitPredicate(opts.wait, res)
		if !ok {
			fmt.Fprintf(cmd.OutOrStderr(),
				"request %s: wait-predicate %q not satisfied: %s\n",
				res.RequestID, opts.wait, why)
			os.Exit(2)
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
}

// checkWaitPredicate returns (satisfied, reason-if-not). The three Phase
// 1.6 predicates are:
//
//   - drain — DrainQuiesced has fired for the request_id.
//   - request_id_terminal — at least one user-domain terminal event
//     (RequestHandled, RequestUnhandled, EventOrphaned, LoopExhausted)
//     has fired for the request_id. Meta-events (EventDispatched,
//     DrainQuiesced, HandlerFailed) don't count — they describe the bus,
//     not user-domain completion.
//   - projection.has=<key> — projection store has the key set for the
//     request_id.
//
// The semantics are: the bus drained, did the predicate close cleanly?
// Async wait (mid-drain blocking) is out of scope for the current
// in-process PoC — a future daemon mode will need this layer, but
// Phase 1.6's CLI wait is a validator over the post-drain state.
func checkWaitPredicate(predicate string, res *runtime.Result) (bool, string) {
	switch {
	case predicate == "drain":
		for _, e := range res.Events {
			if e.Type == projection.TypeDrainQuiesced && e.RequestID == res.RequestID {
				return true, ""
			}
		}
		return false, "no DrainQuiesced for this request_id"
	case predicate == "request_id_terminal":
		for _, e := range res.Events {
			if e.RequestID != res.RequestID || !e.Terminal {
				continue
			}
			switch e.Type {
			case projection.TypeEventDispatched, projection.TypeDrainQuiesced, projection.TypeHandlerFailed:
				continue
			}
			return true, ""
		}
		return false, "no user-domain terminal event for this request_id"
	case strings.HasPrefix(predicate, "projection.has="):
		key := strings.TrimPrefix(predicate, "projection.has=")
		if key == "" {
			return false, "empty key after projection.has="
		}
		if res.Projection != nil && res.Projection.Has(res.RequestID, key) {
			return true, ""
		}
		return false, fmt.Sprintf("projection key %q absent", key)
	}
	return false, fmt.Sprintf("unknown wait-predicate %q", predicate)
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
