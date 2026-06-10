package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/projection"
)

// aggregatorConfig is the YAML body of an aggregator handler.
//
// expected_from names the FAN-OUT event type whose EventDispatched
// meta-event carries the subscriber_count we should wait for. Typically
// this is the request-side event upstream of the per-classifier responses
// the aggregator is collecting.
//
// emits is the aggregated event type fired once enough responses have
// accumulated. Marked terminal: the aggregated event is a leaf
// observation; the aggregator is itself the terminator for the fan-out
// chain (downstream handlers may still react to it, but the aggregator
// closes its own causal arm).
type aggregatorConfig struct {
	ExpectedFrom string `yaml:"expected_from" json:"expected_from"`
	Emit         string `yaml:"emit" json:"emit"`
}

// aggregatorSub is the runtime instance: per-request item buckets, an
// expected-count derived from EventDispatched of the fan-out type, and a
// fired set so the aggregated event is emitted exactly once per request.
type aggregatorSub struct {
	baseSub
	consumes     string
	expectedFrom string
	emit         string

	mu       sync.Mutex
	items    map[string][]json.RawMessage // request_id -> list of received item payloads
	expected map[string]int               // request_id -> subscriber_count from EventDispatched
	fired    map[string]bool              // request_id -> aggregated event already emitted?
}

// newAggregator constructs an aggregator handler from its YAML config.
func newAggregator(cfg config.HandlerConfig) (bus.Subscriber, error) {
	var ac aggregatorConfig
	if err := decodeConfig(cfg.Config, &ac); err != nil {
		return nil, fmt.Errorf("aggregator %q: %w", cfg.Name, err)
	}
	if cfg.On == "" {
		return nil, fmt.Errorf("aggregator %q: on (the response type) is required", cfg.Name)
	}
	if ac.Emit == "" {
		return nil, fmt.Errorf("aggregator %q: config.emit (the aggregated type) is required", cfg.Name)
	}
	if ac.ExpectedFrom == "" {
		return nil, fmt.Errorf("aggregator %q: config.expected_from is required", cfg.Name)
	}
	return &aggregatorSub{
		baseSub:      baseSub{name: cfg.Name},
		consumes:     cfg.On,
		expectedFrom: ac.ExpectedFrom,
		emit:         ac.Emit,
		items:        map[string][]json.RawMessage{},
		expected:     map[string]int{},
		fired:        map[string]bool{},
	}, nil
}

// Match catches both the response type the aggregator is consuming and the
// fan-out EventDispatched meta-event whose subscriber_count tells it how
// many responses to expect.
func (a *aggregatorSub) Match(ev event.Event) bool {
	if ev.Type == a.consumes {
		return true
	}
	if ev.Type == projection.TypeEventDispatched {
		var p struct {
			EventType string `json:"event_type"`
		}
		if err := ev.PayloadAs(&p); err != nil {
			return false
		}
		return p.EventType == a.expectedFrom
	}
	return false
}

// React is where the aggregation happens.
//
// Two paths:
//   - EventDispatched of the fan-out: record subscriber_count for this request.
//   - response event (a.consumes): append payload to items[request_id].
//
// In either path, check whether we've reached expected_count (and haven't
// already fired). If so, emit the aggregated event with items array.
func (a *aggregatorSub) React(_ context.Context, ev event.Event, _ []event.Event) ([]event.Event, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	reqID := ev.RequestID
	if a.fired[reqID] {
		return nil, nil
	}

	switch ev.Type {
	case projection.TypeEventDispatched:
		var p struct {
			EventType       string `json:"event_type"`
			SubscriberCount int    `json:"subscriber_count"`
		}
		if err := ev.PayloadAs(&p); err != nil {
			return nil, fmt.Errorf("aggregator %q: decode EventDispatched: %w", a.name, err)
		}
		if p.EventType != a.expectedFrom {
			return nil, nil
		}
		a.expected[reqID] = p.SubscriberCount
	default:
		// response event of type a.consumes
		a.items[reqID] = append(a.items[reqID], append(json.RawMessage(nil), ev.Payload...))
	}

	expected, haveExpected := a.expected[reqID]
	if !haveExpected || expected <= 0 {
		return nil, nil
	}
	if len(a.items[reqID]) < expected {
		return nil, nil
	}

	a.fired[reqID] = true
	itemsPayload := a.items[reqID]
	// Build the aggregated payload: { "items": [...] }.
	out := struct {
		Items []json.RawMessage `json:"items"`
		Count int               `json:"count"`
	}{Items: itemsPayload, Count: len(itemsPayload)}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("aggregator %q: marshal aggregated: %w", a.name, err)
	}
	return []event.Event{{
		Type:     a.emit,
		Payload:  raw,
		Terminal: true,
	}}, nil
}

// aggregatorSpec is the type-level HandlerSpec for the registry.
// Consumes and Emits resolve from config per instance via
// aggregatorSpecResolver.
func aggregatorSpec() HandlerSpec {
	return HandlerSpec{
		Type:        "aggregator",
		Description: "collects N responses (count from EventDispatched.subscriber_count of a fan-out event) and emits an aggregated event once",
		Consumes:    "*",
		Emits:       nil,
	}
}

// aggregatorSpecResolver substitutes the configured emit type so the
// static graph builder sees the actual emission for cycle / shape
// validation.
func aggregatorSpecResolver(cfg config.HandlerConfig, base HandlerSpec) HandlerSpec {
	resolved := base
	if cfg.Config != nil {
		if v, ok := cfg.Config["emit"].(string); ok && v != "" {
			resolved.Emits = []EmittedSpec{{Type: v, Terminal: true, Optional: false}}
		}
	}
	return resolved
}
