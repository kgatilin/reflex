// Package bus is reflex's in-memory dispatcher.
//
// The dispatcher is intentionally not a goroutine loop. It is a drain
// function: given a seed event, append it to the store, fan out to matching
// subscribers, append any events they emit, and repeat until the queue is
// empty. The "no agent loop" thesis of reflex is about the absence of a
// monolithic orchestrator, not the absence of for-loops in the substrate.
//
// Subscribers are pure reactors: they receive an event plus a read-only view
// of the store and return zero or more new events. They hold no state of
// their own — anything they need they project from the log.
package bus

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/kgatilin/reflex/pkg/event"
)

// Subscriber reacts to a single event. It must be a pure function of the
// event plus the store snapshot it is given; it must not mutate the store
// directly. Returned events are appended by the dispatcher.
type Subscriber interface {
	Name() string
	// Match decides whether this subscriber wants to react to ev. The
	// dispatcher calls Match exactly once per event per subscriber.
	Match(ev event.Event) bool
	// React is invoked only when Match returned true. It receives a
	// snapshot of all events appended so far (including ev) and returns
	// zero or more new events to be appended.
	React(ctx context.Context, ev event.Event, log []event.Event) ([]event.Event, error)
}

// Bus owns the event store and the subscriber registry. Dispatch is
// single-threaded by design: each event drains its consequences before the
// next event is processed. That gives reflex deterministic event ordering
// without an explicit scheduler.
type Bus struct {
	store       *event.Store
	subscribers []Subscriber
	source      string
	maxSteps    int
	clock       func() time.Time
}

// Option customises a Bus.
type Option func(*Bus)

// WithMaxSteps caps the number of dispatch iterations per Run. Without this,
// a misconfigured handler chain can loop forever; reflex prefers a bounded
// abort to a hung process.
func WithMaxSteps(n int) Option {
	return func(b *Bus) { b.maxSteps = n }
}

// WithSource sets the default source attribution applied to seed events.
func WithSource(s string) Option {
	return func(b *Bus) { b.source = s }
}

// WithClock overrides time.Now for tests.
func WithClock(c func() time.Time) Option {
	return func(b *Bus) { b.clock = c }
}

// New constructs a Bus over store.
func New(store *event.Store, opts ...Option) *Bus {
	b := &Bus{
		store:    store,
		source:   "reflex",
		maxSteps: 256,
		clock:    func() time.Time { return time.Now().UTC() },
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

// Register adds sub to the bus. Subscribers fire in registration order when
// multiple match a single event.
func (b *Bus) Register(sub Subscriber) {
	b.subscribers = append(b.subscribers, sub)
}

// Subscribers returns the registered list in registration order.
func (b *Bus) Subscribers() []Subscriber {
	out := make([]Subscriber, len(b.subscribers))
	copy(out, b.subscribers)
	return out
}

// Store returns the underlying event store.
func (b *Bus) Store() *event.Store { return b.store }

// Emit decorates a partial event with a fresh ID, timestamp, and default
// source, appends it, and returns the finalised event. Handlers should not
// call Emit directly — they return events from React and let the dispatcher
// append them. Emit is the entry point for seed events from the CLI.
func (b *Bus) Emit(ev event.Event) event.Event {
	if ev.ID == "" {
		ev.ID = uuid.NewString()
	}
	if ev.TS.IsZero() {
		ev.TS = b.clock()
	}
	if ev.Source == "" {
		ev.Source = b.source
	}
	b.store.Append(ev)
	return ev
}

// Run drains all reactions triggered by seed and any descendants. It returns
// when the queue is empty (quiescence) or when MaxSteps is hit.
//
// The drain is intentionally a plain loop, not a goroutine pool: reflex
// wants deterministic ordering and a clean trace.
func (b *Bus) Run(ctx context.Context, seed event.Event) error {
	if b.store == nil {
		return errors.New("bus: store is nil")
	}

	first := b.Emit(seed)
	queue := []event.Event{first}
	steps := 0

	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if steps >= b.maxSteps {
			return fmt.Errorf("bus: max steps (%d) exceeded — likely a handler loop", b.maxSteps)
		}
		steps++

		ev := queue[0]
		queue = queue[1:]

		// Snapshot here so all subscribers reacting to ev see the same log
		// state. New events appended by earlier subscribers in this fan-out
		// are not visible to later ones at the same depth — they show up on
		// the next iteration. This keeps causal ordering clean.
		snap := b.store.Snapshot()

		for _, sub := range b.subscribers {
			if !sub.Match(ev) {
				continue
			}
			emitted, err := sub.React(ctx, ev, snap)
			if err != nil {
				return fmt.Errorf("bus: subscriber %q on %q: %w", sub.Name(), ev.Type, err)
			}
			for _, ne := range emitted {
				if ne.CausedBy == "" {
					ne.CausedBy = ev.ID
				}
				if ne.RequestID == "" {
					ne.RequestID = ev.RequestID
				}
				if ne.Source == "" {
					ne.Source = sub.Name()
				}
				ne.ID = uuid.NewString()
				ne.TS = b.clock()
				b.store.Append(ne)
				queue = append(queue, ne)
			}
		}
	}

	return nil
}
