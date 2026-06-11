// Command fs is the doc-14 file-tools plugin: it connects to a running
// `reflex daemon`, registers one handler per tool kind, and serves
// tool.fs.read / tool.fs.edit / tool.fs.write / tool.fs.search rooted at
// --root. Confinement is plugin rooting, not a bus gate — every path is
// clamped under the root and an escape becomes a tool.fs.*.failed event.
//
//	plugins/fs --socket /tmp/reflex.sock --root /abs/path/to/workspace
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/kgatilin/reflex/pkg/sdk"
)

func main() {
	socket := flag.String("socket", "/tmp/reflex.sock", "path to the reflex daemon's Unix socket")
	root := flag.String("root", "", "workspace root every path is clamped under (required)")
	flag.Parse()

	if *root == "" {
		fmt.Fprintln(os.Stderr, "fs: --root is required")
		os.Exit(1)
	}
	tools, err := newFSTools(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fs:", err)
		os.Exit(1)
	}

	client, err := sdk.Connect(sdk.Remote(*socket))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fs: connect:", err)
		os.Exit(1)
	}
	defer client.Close()

	for _, reg := range []struct {
		kind string // fs.read → tool.fs.read.call / .result / .failed
		fn   func(json.RawMessage) (any, error)
	}{
		{"fs.read", func(raw json.RawMessage) (any, error) {
			var p readParams
			if err := unmarshalParams(raw, &p); err != nil {
				return nil, err
			}
			return tools.read(p)
		}},
		{"fs.edit", func(raw json.RawMessage) (any, error) {
			var p editParams
			if err := unmarshalParams(raw, &p); err != nil {
				return nil, err
			}
			return tools.edit(p)
		}},
		{"fs.write", func(raw json.RawMessage) (any, error) {
			var p writeParams
			if err := unmarshalParams(raw, &p); err != nil {
				return nil, err
			}
			return tools.write(p)
		}},
		{"fs.search", func(raw json.RawMessage) (any, error) {
			var p searchParams
			if err := unmarshalParams(raw, &p); err != nil {
				return nil, err
			}
			return tools.search(p)
		}},
	} {
		if err := client.Register(toolHandler(reg.kind, reg.fn)); err != nil {
			fmt.Fprintf(os.Stderr, "fs: register %s: %v\n", reg.kind, err)
			os.Exit(1)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	fmt.Fprintf(os.Stderr, "fs: connected to %s, rooted at %s\n", *socket, tools.root)
	if err := client.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "fs: run:", err)
		os.Exit(1)
	}
}

// toolHandler wraps one tool function in the uniform call→result|failed
// shape: tool.<kind>.call consumed, tool.<kind>.result emitted on success,
// tool.<kind>.failed{error} on any rejection. Failures are events the
// topology reacts to, never handler errors (a handler error would surface as
// HandlerFailed and stall the loop).
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
