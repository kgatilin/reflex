// Command gotool is the doc-14 validator plugin: deterministic wrappers for
// go build / go test / go vet served as tool.go.*.call handlers, rooted at
// --root. Their results in the fold are what the operator (and later a gate)
// judges work by.
//
//	plugins/gotool --socket /tmp/reflex.sock --root /abs/path/to/workspace
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kgatilin/reflex/pkg/sdk"
)

func main() {
	socket := flag.String("socket", "/tmp/reflex.sock", "path to the reflex daemon's Unix socket")
	root := flag.String("root", "", "module root go commands run in (required)")
	timeout := flag.Duration("timeout", 3*time.Minute, "per-call timeout for go commands")
	flag.Parse()

	if *root == "" {
		fmt.Fprintln(os.Stderr, "gotool: --root is required")
		os.Exit(1)
	}
	abs, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gotool:", err)
		os.Exit(1)
	}
	tools := &goTools{root: abs, timeout: *timeout}

	client, err := sdk.Connect(sdk.Remote(*socket))
	if err != nil {
		fmt.Fprintln(os.Stderr, "gotool: connect:", err)
		os.Exit(1)
	}
	defer client.Close()

	for _, reg := range []struct {
		kind string
		fn   func(json.RawMessage) (any, error)
	}{
		{"go.build", func(raw json.RawMessage) (any, error) {
			var p buildParams
			if err := unmarshalParams(raw, &p); err != nil {
				return nil, err
			}
			return tools.build(p)
		}},
		{"go.test", func(raw json.RawMessage) (any, error) {
			var p testParams
			if err := unmarshalParams(raw, &p); err != nil {
				return nil, err
			}
			return tools.test(p)
		}},
		{"go.vet", func(raw json.RawMessage) (any, error) {
			var p vetParams
			if err := unmarshalParams(raw, &p); err != nil {
				return nil, err
			}
			return tools.vet(p)
		}},
	} {
		if err := client.Register(toolHandler(reg.kind, reg.fn)); err != nil {
			fmt.Fprintf(os.Stderr, "gotool: register %s: %v\n", reg.kind, err)
			os.Exit(1)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	fmt.Fprintf(os.Stderr, "gotool: connected to %s, rooted at %s\n", *socket, abs)
	if err := client.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "gotool: run:", err)
		os.Exit(1)
	}
}

// toolHandler wraps one tool function in the uniform call→result|failed
// shape (same contract as the fs plugin): success and verdicts are
// tool.<kind>.result; only an unusable wrapper emits tool.<kind>.failed.
func toolHandler(kind string, fn func(json.RawMessage) (any, error)) *sdk.Handler {
	resultType := "tool." + kind + ".result"
	failedType := "tool." + kind + ".failed"
	return sdk.NewHandler("plugin-"+kind,
		sdk.Consumes("tool."+kind+".call"),
		sdk.Emits(resultType),
		sdk.Emits(failedType),
	).OnEvent(func(ctx sdk.Ctx, ev sdk.Event) error {
		out, err := fn(ev.Payload)
		if err != nil {
			return ctx.Emit(failedType, sdk.Args{"error": err.Error()})
		}
		payload, err := json.Marshal(out)
		if err != nil {
			return ctx.Emit(failedType, sdk.Args{"error": err.Error()})
		}
		return ctx.EmitRaw(resultType, payload)
	})
}

func unmarshalParams(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("bad call payload: %w", err)
	}
	return nil
}
