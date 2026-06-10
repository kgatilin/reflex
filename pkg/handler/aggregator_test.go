package handler

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/projection"
)

func TestAggregatorFiresAfterExpectedCount(t *testing.T) {
	cfg := config.HandlerConfig{
		Name: "collect",
		Type: "aggregator",
		On:   "Classification",
		Config: map[string]any{
			"expected_from": "ClassifyRequested",
			"emit":          "ClassificationsAggregated",
		},
	}
	sub, err := newAggregator(cfg)
	if err != nil {
		t.Fatalf("newAggregator: %v", err)
	}

	// Step 1: tell the aggregator there will be 3 classifiers via the
	// EventDispatched meta-event for ClassifyRequested.
	edPayload, _ := json.Marshal(map[string]any{
		"event_type":       "ClassifyRequested",
		"subscriber_count": 3,
	})
	emitted, err := sub.React(context.Background(),
		event.Event{
			Type:      projection.TypeEventDispatched,
			RequestID: "r",
			Payload:   edPayload,
		}, nil)
	if err != nil {
		t.Fatalf("React EventDispatched: %v", err)
	}
	if len(emitted) != 0 {
		t.Fatalf("EventDispatched alone should not trigger emission, got %+v", emitted)
	}

	// Step 2: feed 3 Classification events. Only the 3rd should produce
	// the aggregated event.
	for i := 1; i <= 3; i++ {
		payload, _ := json.Marshal(map[string]any{"label": i})
		emitted, err = sub.React(context.Background(),
			event.Event{Type: "Classification", RequestID: "r", Payload: payload}, nil)
		if err != nil {
			t.Fatalf("React Classification %d: %v", i, err)
		}
		if i < 3 && len(emitted) != 0 {
			t.Fatalf("aggregator fired too early at i=%d: %+v", i, emitted)
		}
		if i == 3 {
			if len(emitted) != 1 {
				t.Fatalf("aggregator should emit one event at i=3, got %d", len(emitted))
			}
			if emitted[0].Type != "ClassificationsAggregated" {
				t.Fatalf("aggregated event type = %q", emitted[0].Type)
			}
			if !emitted[0].Terminal {
				t.Fatal("aggregated event must be terminal")
			}
			var out struct {
				Items []json.RawMessage `json:"items"`
				Count int               `json:"count"`
			}
			if err := json.Unmarshal(emitted[0].Payload, &out); err != nil {
				t.Fatalf("decode aggregated: %v", err)
			}
			if out.Count != 3 || len(out.Items) != 3 {
				t.Fatalf("aggregated items count = %d", out.Count)
			}
		}
	}
}

func TestAggregatorFiresExactlyOnce(t *testing.T) {
	cfg := config.HandlerConfig{
		Name: "collect",
		Type: "aggregator",
		On:   "X",
		Config: map[string]any{
			"expected_from": "Fanout",
			"emit":          "XAgg",
		},
	}
	sub, _ := newAggregator(cfg)

	ed, _ := json.Marshal(map[string]any{"event_type": "Fanout", "subscriber_count": 2})
	if _, err := sub.React(context.Background(),
		event.Event{Type: projection.TypeEventDispatched, RequestID: "r", Payload: ed}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := sub.React(context.Background(),
		event.Event{Type: "X", RequestID: "r", Payload: []byte(`{}`)}, nil); err != nil {
		t.Fatal(err)
	}
	first, err := sub.React(context.Background(),
		event.Event{Type: "X", RequestID: "r", Payload: []byte(`{}`)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Fatalf("expected one emission, got %d", len(first))
	}

	// One more Classification — must not re-fire.
	second, err := sub.React(context.Background(),
		event.Event{Type: "X", RequestID: "r", Payload: []byte(`{}`)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("aggregator fired again: %+v", second)
	}
}

func TestAggregatorRegisteredInBuiltinRegistry(t *testing.T) {
	r := BuiltinRegistry()
	if _, ok := r.SpecOf("aggregator"); !ok {
		t.Fatal("aggregator missing from BuiltinRegistry")
	}
}

func TestAggregatorEndToEndFanout(t *testing.T) {
	// Build a tiny bus with 3 echo classifiers and one aggregator, drive
	// it with a single ClassifyRequested, assert the aggregator fires
	// exactly one ClassificationsAggregated.
	store := event.NewStore()
	b := bus.New(store)

	// Three classifiers: each consumes ClassifyRequested and emits
	// Classification with its own label.
	for i := 0; i < 3; i++ {
		sub := makeStaticEmitter("classify-"+itoa(i), "ClassifyRequested", "Classification",
			map[string]any{"label": i})
		b.Register(sub)
	}

	aggCfg := config.HandlerConfig{
		Name: "collect",
		Type: "aggregator",
		On:   "Classification",
		Config: map[string]any{
			"expected_from": "ClassifyRequested",
			"emit":          "ClassificationsAggregated",
		},
	}
	agg, err := newAggregator(aggCfg)
	if err != nil {
		t.Fatal(err)
	}
	b.Register(agg)

	if err := b.Run(context.Background(),
		event.Event{Type: "ClassifyRequested", RequestID: "r", Payload: []byte(`{}`)}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := 0
	for _, e := range store.Snapshot() {
		if e.Type == "ClassificationsAggregated" {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("ClassificationsAggregated count = %d, want 1", got)
	}
}

// makeStaticEmitter builds a subscriber that consumes `on` and emits one
// event of type `emit` with a fixed payload. Used for the end-to-end
// aggregator test.
func makeStaticEmitter(name, on, emit string, payload map[string]any) bus.Subscriber {
	raw, _ := json.Marshal(payload)
	return &genericSub{
		baseSub: baseSub{name: name},
		on:      on,
		run: func(_ context.Context, _ event.Event, _ []event.Event) ([]event.Event, error) {
			return []event.Event{{Type: emit, Payload: raw}}, nil
		},
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
