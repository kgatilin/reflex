package handler

import (
	"context"
	"encoding/json"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/projection"
)

// CheckQuiescence runs after the dispatcher's main drain terminates. For
// every request observed in the log that did not produce a RequestHandled
// event, it emits a RequestUnhandled event so the unresolved state is
// visible in the trace.
//
// We could implement this as a Subscriber too — every event matches, we
// scan after every step — but it would dominate the trace with noise. A
// post-drain pass is the simpler shape.
func CheckQuiescence(ctx context.Context, b *bus.Bus) error {
	store := b.Store()
	seen := map[string]bool{}
	for _, e := range store.Snapshot() {
		if e.RequestID == "" {
			continue
		}
		if seen[e.RequestID] {
			continue
		}
		seen[e.RequestID] = true
		state := projection.SessionProjection(store.Snapshot(), e.RequestID)
		if state.Handled || state.Unhandled {
			continue
		}
		payload, err := json.Marshal(map[string]string{
			"reason": "drain quiesced without RequestHandled",
		})
		if err != nil {
			return err
		}
		b.Emit(event.Event{
			Type:      projection.TypeRequestUnhandled,
			RequestID: e.RequestID,
			Source:    "unhandled_watcher",
			Payload:   payload,
		})
	}
	return nil
}

// newUnhandledWatcher exists so configs can declare an "unhandled_watcher"
// handler explicitly. The actual logic runs from CheckQuiescence; this
// handler is a documentation aid — it does nothing on event dispatch.
//
// Declaring it in YAML makes it visible to readers of the config; not
// declaring it does not disable the post-drain check.
func newUnhandledWatcher(cfg config.HandlerConfig) (bus.Subscriber, error) {
	on := cfg.On
	if on == "" {
		on = "__noop__"
	}
	return &genericSub{
		baseSub: baseSub{name: cfg.Name},
		on:      on,
		run: func(_ context.Context, _ event.Event, _ []event.Event) ([]event.Event, error) {
			return nil, nil
		},
	}, nil
}

// Compile-time assertion.
var _ bus.Subscriber = (*genericSub)(nil)
