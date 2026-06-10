// Package runtime wires a YAML config into a live bus.
//
// It is internal because the wiring is not a public API: callers go through
// cmd/reflex or write their own glue.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/graph"
	"github.com/kgatilin/reflex/pkg/handler"
	"github.com/kgatilin/reflex/pkg/projection"
)

// Result is what the runtime returns to the CLI after a single user message.
type Result struct {
	RequestID string
	Events    []event.Event
	State     projection.SessionState
}

// Build constructs a bus from cfg with handlers built via the supplied
// registry. The bus is fully wired but no events have been emitted yet.
//
// Build also compiles the static handler graph and refuses to return a bus
// when the graph contains an uncapped cycle: reflex would rather refuse to
// start than silently loop forever. Declared loops (cycles with a
// max_iterations cap) are honoured by installing per-handler caps on the
// dispatcher.
func Build(cfg *config.File, reg *handler.Registry) (*bus.Bus, error) {
	g, err := graph.Build(cfg, reg)
	if err != nil {
		return nil, err
	}
	store := event.NewStore()
	opts := []bus.Option{}
	if cfg.Settings.MaxSteps > 0 {
		opts = append(opts, bus.WithMaxSteps(cfg.Settings.MaxSteps))
	}
	if caps := g.Caps(); len(caps) > 0 {
		opts = append(opts, bus.WithLoopCaps(caps))
	}
	b := bus.New(store, opts...)
	for _, h := range cfg.Handlers {
		sub, err := reg.Build(h)
		if err != nil {
			return nil, fmt.Errorf("runtime: build %q: %w", h.Name, err)
		}
		b.Register(sub)
	}
	return b, nil
}

// Run seeds a RequestReceived event with the user's message, drains the
// bus, and runs CheckQuiescence to flag unhandled requests.
func Run(ctx context.Context, b *bus.Bus, message string) (*Result, error) {
	reqID := uuid.NewString()
	payload, err := json.Marshal(map[string]string{"payload": message})
	if err != nil {
		return nil, err
	}
	seed := event.Event{
		Type:      projection.TypeRequestReceived,
		RequestID: reqID,
		Source:    "cli",
		Payload:   payload,
	}
	if err := b.Run(ctx, seed); err != nil {
		return nil, err
	}
	if err := handler.CheckQuiescence(ctx, b); err != nil {
		return nil, err
	}
	all := b.Store().Snapshot()
	state := projection.SessionProjection(all, reqID)
	return &Result{RequestID: reqID, Events: all, State: state}, nil
}
