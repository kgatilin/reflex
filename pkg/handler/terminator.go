package handler

import (
	"context"
	"fmt"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/projection"
)

// terminator emits RequestHandled when the event it watches fires — useful
// for declaratively closing a request from any event type without involving
// the LLM stub. Idempotent: if RequestHandled is already in the log for this
// request, it does nothing.
func newTerminator(cfg config.HandlerConfig) (bus.Subscriber, error) {
	on := cfg.On
	if on == "" {
		return nil, fmt.Errorf("terminator %q: on is required", cfg.Name)
	}
	return &genericSub{
		baseSub: baseSub{name: cfg.Name},
		on:      on,
		run: func(_ context.Context, ev event.Event, log []event.Event) ([]event.Event, error) {
			state := projection.SessionProjection(log, ev.RequestID)
			if state.Handled {
				return nil, nil
			}
			return []event.Event{{Type: projection.TypeRequestHandled}}, nil
		},
	}, nil
}

// Compile-time assertion.
var _ bus.Subscriber = (*genericSub)(nil)
