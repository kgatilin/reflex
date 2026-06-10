package handler

import (
	"context"
	"encoding/json"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/projection"
)

// CheckQuiescence runs after the dispatcher's main drain terminates. It
// implements two diagnostics that together pin down "what went wrong" when
// the bus quiesces in an unexpected state:
//
//  1. For every observed request that did NOT produce RequestHandled,
//     emit RequestUnhandled (request-level diagnostic — unchanged from v1).
//  2. For every non-terminal event with zero descendants by `caused_by`,
//     emit EventOrphaned (event-level diagnostic — the Phase 1 invariant).
//
// EventOrphaned itself is terminal so it cannot trigger another orphan
// complaint. The orphan scan deliberately skips the most recent batch of
// EventOrphaned events themselves (which are leaves by design).
//
// We run as a single post-drain pass rather than as a Subscriber matching
// every event, because the drain-time view is the only point at which "this
// event has no children" is a stable claim.
func CheckQuiescence(ctx context.Context, b *bus.Bus) error {
	store := b.Store()
	snapshot := store.Snapshot()

	// Pass 1: request-level — RequestUnhandled per request without RequestHandled.
	seen := map[string]bool{}
	for _, e := range snapshot {
		if e.RequestID == "" {
			continue
		}
		if seen[e.RequestID] {
			continue
		}
		seen[e.RequestID] = true
		state := projection.SessionProjection(snapshot, e.RequestID)
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
			Terminal:  true,
			Payload:   payload,
		})
	}

	// Re-snapshot so the orphan pass sees the diagnostics it just emitted —
	// otherwise RequestUnhandled itself would be flagged as orphan-able.
	snapshot = store.Snapshot()
	childCount := map[string]int{}
	for _, e := range snapshot {
		if e.CausedBy != "" {
			childCount[e.CausedBy]++
		}
	}

	// Pass 2: event-level — non-terminal events with zero descendants.
	for _, e := range snapshot {
		if e.Terminal {
			continue
		}
		if childCount[e.ID] > 0 {
			continue
		}
		payload, err := json.Marshal(map[string]string{
			"orphan_id":   e.ID,
			"orphan_type": e.Type,
			"request_id":  e.RequestID,
			"reason":      "non-terminal event with no descendants",
		})
		if err != nil {
			return err
		}
		b.Emit(event.Event{
			Type:      projection.TypeEventOrphaned,
			RequestID: e.RequestID,
			Source:    "unhandled_watcher",
			CausedBy:  e.ID,
			Terminal:  true,
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
