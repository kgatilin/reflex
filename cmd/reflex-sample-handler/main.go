// Command reflex-sample-handler is the Phase 4a end-to-end example: a tiny
// standalone Go binary that connects to a running `reflex daemon` over a
// Unix socket, registers itself as a handler reacting to RequestReceived,
// and emits ResponseEmitted back into the bus.
//
// Pair with examples/distributed_triage.yaml:
//
//	# terminal 1
//	reflex daemon --config examples/distributed_triage.yaml --socket /tmp/reflex-demo.sock
//
//	# terminal 2
//	reflex-sample-handler --socket /tmp/reflex-demo.sock
//
//	# terminal 3
//	reflex emit --type RequestReceived --payload '{"payload":"hello"}' \
//	    --daemon /tmp/reflex-demo.sock
//
// The sample handler is intentionally trivial — its purpose is to show the
// SDK call site, not to do interesting work.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/kgatilin/reflex/pkg/sdk"
)

func main() {
	socket := flag.String("socket", "/tmp/reflex.sock", "path to the reflex daemon's Unix socket")
	name := flag.String("name", "sample-handler", "handler name (visible in the trace)")
	flag.Parse()

	client, err := sdk.Connect(sdk.Remote(*socket))
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer client.Close()

	h := sdk.NewHandler(*name,
		sdk.Consumes("RequestReceived"),
		sdk.Emits("ResponseEmitted"),
		sdk.Terminal("ResponseEmitted"),
	).OnEvent(func(ctx sdk.Ctx, ev sdk.Event) error {
		// Decode the request payload, uppercase the text, and emit a
		// response. Pure pub/sub: no waiting, no return value.
		var p struct {
			Payload string `json:"payload"`
		}
		_ = ev.PayloadAs(&p)
		reply := "echo: " + strings.ToUpper(p.Payload)
		return ctx.Emit("ResponseEmitted", sdk.Args{"text": reply})
	})
	if err := client.Register(h); err != nil {
		fmt.Fprintln(os.Stderr, "register:", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	fmt.Fprintf(os.Stderr, "reflex-sample-handler: connected to %s as %q — waiting for events\n", *socket, *name)
	if err := client.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "reflex-sample-handler: stopped")
}
