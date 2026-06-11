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
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/projection"
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

// LoopExhaustedType is the event type the dispatcher emits when a declared
// loop hits its max_iterations cap. The projection layer re-exports the
// same literal as projection.TypeLoopExhausted.
const LoopExhaustedType = "LoopExhausted"

// Phase 1.6 bus meta-event types — the bus is the source of these. They
// describe the bus's own activity (routing, drain, failure) and are
// first-class events on the same log. All three are terminal.
const (
	EventDispatchedType = "EventDispatched"
	DrainQuiescedType   = "DrainQuiesced"
	HandlerFailedType   = "HandlerFailed"
)

// Bus owns the event store and the subscriber registry. Dispatch is
// single-threaded by design: each event drains its consequences before the
// next event is processed. That gives reflex deterministic event ordering
// without an explicit scheduler.
type Bus struct {
	store *event.Store
	// subsMu protects subscribers against concurrent Register from
	// multiple connections in daemon mode. Pre-Phase-4a use was strictly
	// single-threaded, so this lock is uncontended in the common path.
	subsMu      sync.RWMutex
	subscribers []Subscriber
	source      string
	maxSteps    int
	clock       func() time.Time
	// loopCaps is the per-handler-name iteration cap. When a subscriber's
	// Name matches a key here, the dispatcher tracks (requestID, name)
	// fire counts and refuses to fire past the cap; it emits a terminal
	// LoopExhausted event instead.
	loopCaps map[string]int
	// proj is the per-request projection store handlers can read/write.
	// May be nil — handlers that touch it through ProjectionAware get a
	// no-op store back in that case.
	proj *projection.Store
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

// WithProjection wires a projection store onto the bus so handlers can
// stash structured intermediate results keyed by request_id. Optional —
// when unset, ProjectionAware handlers see a nil store and skip the
// projection step gracefully.
func WithProjection(p *projection.Store) Option {
	return func(b *Bus) { b.proj = p }
}

// WithLoopCaps installs per-handler iteration caps. Used by the runtime
// after compiling the YAML into a HandlerGraph: each loop-declared handler
// is registered here so the dispatcher will refuse to over-fire it.
//
// The caller passes a copy of the map; later mutations don't affect the
// bus. Passing nil clears any previously installed caps.
func WithLoopCaps(caps map[string]int) Option {
	return func(b *Bus) {
		if caps == nil {
			b.loopCaps = nil
			return
		}
		out := make(map[string]int, len(caps))
		for k, v := range caps {
			out[k] = v
		}
		b.loopCaps = out
	}
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
// multiple match a single event. Safe to call concurrently — Phase 4a
// daemon mode invokes this from each accepted connection.
func (b *Bus) Register(sub Subscriber) {
	b.subsMu.Lock()
	defer b.subsMu.Unlock()
	b.subscribers = append(b.subscribers, sub)
}

// Subscribers returns the registered list in registration order. Safe to
// call concurrently with Register.
func (b *Bus) Subscribers() []Subscriber {
	b.subsMu.RLock()
	defer b.subsMu.RUnlock()
	out := make([]Subscriber, len(b.subscribers))
	copy(out, b.subscribers)
	return out
}

// snapshotSubscribers takes a copy of the current subscriber list for the
// dispatcher. The drain loop iterates this snapshot — late-arriving
// Registers are picked up on the NEXT drain iteration, not mid-loop, so
// fan-out ordering remains deterministic for any given event.
func (b *Bus) snapshotSubscribers() []Subscriber {
	return b.Subscribers()
}

// Store returns the underlying event store.
func (b *Bus) Store() *event.Store { return b.store }

// Projection returns the bus's projection store. May be nil if WithProjection
// was not supplied; callers should nil-check or use the store's nil-safe
// methods.
func (b *Bus) Projection() *projection.Store { return b.proj }

// ProjectionAware is implemented by subscribers that want a handle to the
// bus's projection store at construction time (so they can Set/Get inside
// React without threading the store through configuration). The runtime
// calls SetProjection once after Register.
type ProjectionAware interface {
	SetProjection(p *projection.Store)
}

// WireProjection injects b's projection store into every registered
// subscriber that implements ProjectionAware. Called by the runtime after
// the bus is fully assembled.
func (b *Bus) WireProjection() {
	if b.proj == nil {
		return
	}
	for _, s := range b.Subscribers() {
		if pa, ok := s.(ProjectionAware); ok {
			pa.SetProjection(b.proj)
		}
	}
}

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
//
// Loop enforcement (Phase 1.5): for every subscriber whose Name is keyed
// in b.loopCaps, the dispatcher tracks a per-(request_id, handler_name)
// fire count. When firing one more time would exceed the cap, the
// subscriber is NOT called; the dispatcher emits a terminal
// LoopExhausted{handler, max_iterations, request_id} event in its place,
// caused-by the trigger event. The terminal flag closes the causal branch
// so the orphan watcher stays silent.
func (b *Bus) Run(ctx context.Context, seed event.Event) error {
	if b.store == nil {
		return errors.New("bus: store is nil")
	}

	first := b.Emit(seed)
	queue := []event.Event{first}
	steps := 0
	// fireCount[reqID][handlerName] = number of times the handler has
	// reacted to an event for this request. Only populated for handlers
	// keyed in b.loopCaps.
	fireCount := map[string]map[string]int{}
	// loopExhaustedFired[reqID][handlerName] = true once we've emitted
	// the diagnostic; we emit at most one per (request, handler) so the
	// trace stays clean.
	loopExhaustedFired := map[string]map[string]bool{}

	// requestsSeen tracks the order in which request_ids first appear in
	// the drain so we can emit DrainQuiesced for each at the end.
	requestsSeen := []string{}
	seenSet := map[string]bool{}

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

		if ev.RequestID != "" && !seenSet[ev.RequestID] {
			seenSet[ev.RequestID] = true
			requestsSeen = append(requestsSeen, ev.RequestID)
		}

		// Snapshot here so all subscribers reacting to ev see the same log
		// state. New events appended by earlier subscribers in this fan-out
		// are not visible to later ones at the same depth — they show up on
		// the next iteration. This keeps causal ordering clean.
		snap := b.store.Snapshot()

		subscriberCount := 0

		// Snapshot once per dispatch step so concurrent Register from
		// remote-handler connections doesn't race with the fan-out loop.
		subsSnap := b.snapshotSubscribers()
		for _, sub := range subsSnap {
			if !sub.Match(ev) {
				continue
			}

			// Loop cap enforcement happens BEFORE the React call.
			if cap, capped := b.loopCaps[sub.Name()]; capped {
				if fireCount[ev.RequestID] == nil {
					fireCount[ev.RequestID] = map[string]int{}
				}
				if fireCount[ev.RequestID][sub.Name()] >= cap {
					// Already at cap — skip and emit diagnostic once.
					if loopExhaustedFired[ev.RequestID] == nil {
						loopExhaustedFired[ev.RequestID] = map[string]bool{}
					}
					if !loopExhaustedFired[ev.RequestID][sub.Name()] {
						loopExhaustedFired[ev.RequestID][sub.Name()] = true
						payload := loopExhaustedPayload(sub.Name(), cap)
						ne := event.Event{
							Type:      LoopExhaustedType,
							RequestID: ev.RequestID,
							CausedBy:  ev.ID,
							Source:    "bus",
							Terminal:  true,
							Payload:   payload,
							ID:        uuid.NewString(),
							TS:        b.clock(),
						}
						b.store.Append(ne)
						// LoopExhausted is terminal and ought not spawn
						// further reactions, so we don't enqueue it.
					}
					continue
				}
				fireCount[ev.RequestID][sub.Name()]++
			}

			subscriberCount++
			emitted, err := sub.React(ctx, ev, snap)
			if err != nil {
				// Emit HandlerFailed as a first-class observable event
				// before returning, so the trace records the failure even
				// when the caller treats the dispatcher error as fatal.
				b.emitHandlerFailed(sub.Name(), ev, err)
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

		// Emit EventDispatched after the fan-out for ev is complete. We
		// skip meta-events themselves (would be infinite). The meta-event
		// is enqueued so subscribers to it (e.g. the generic aggregator)
		// get to react — and the drain continues until quiescence.
		if !isMetaEventType(ev.Type) {
			if me := b.emitEventDispatched(ev, subscriberCount); me != nil {
				queue = append(queue, *me)
			}
		}
	}

	// Drain finished — emit one DrainQuiesced per request_id seen. These
	// are leaf observations; we don't re-enter the drain for them.
	for _, rid := range requestsSeen {
		b.emitDrainQuiesced(rid)
	}

	return nil
}

// isMetaEventType reports whether typ is one of the bus's own meta-events.
// Used to break the recursion: the bus never emits a meta-event about
// another meta-event.
func isMetaEventType(typ string) bool {
	switch typ {
	case EventDispatchedType, DrainQuiescedType, HandlerFailedType, LoopExhaustedType:
		return true
	}
	return false
}

// emitEventDispatched appends an EventDispatched meta-event describing the
// fan-out for trigger and returns it so the caller can enqueue it for
// dispatch (so aggregators subscribed to EventDispatched fire).
//
// The meta-event is terminal: it is a leaf observation about a routing
// step. Terminal-ness governs the orphan watcher, not the drain — the
// drain still routes it to subscribers, the orphan check then ignores it.
func (b *Bus) emitEventDispatched(trigger event.Event, count int) *event.Event {
	payload := []byte(fmt.Sprintf(`{"event_type":%q,"subscriber_count":%d}`,
		trigger.Type, count))
	ne := event.Event{
		ID:        uuid.NewString(),
		Type:      EventDispatchedType,
		RequestID: trigger.RequestID,
		TS:        b.clock(),
		Source:    "bus",
		CausedBy:  trigger.ID,
		Terminal:  true,
		Payload:   payload,
	}
	b.store.Append(ne)
	return &ne
}

// emitDrainQuiesced records that no work remains for requestID. Emitted
// after the queue empties; not enqueued (drain is already finishing).
func (b *Bus) emitDrainQuiesced(requestID string) {
	payload := []byte(fmt.Sprintf(`{"request_id":%q}`, requestID))
	ne := event.Event{
		ID:        uuid.NewString(),
		Type:      DrainQuiescedType,
		RequestID: requestID,
		TS:        b.clock(),
		Source:    "bus",
		Terminal:  true,
		Payload:   payload,
	}
	b.store.Append(ne)
}

// emitHandlerFailed records a handler raising an error. We capture handler
// name, the event type that triggered the failure, and the error string.
// Not enqueued: the dispatcher is about to abort.
func (b *Bus) emitHandlerFailed(handlerName string, trigger event.Event, err error) {
	payload := []byte(fmt.Sprintf(`{"handler_name":%q,"event_type":%q,"error":%q}`,
		handlerName, trigger.Type, err.Error()))
	ne := event.Event{
		ID:        uuid.NewString(),
		Type:      HandlerFailedType,
		RequestID: trigger.RequestID,
		TS:        b.clock(),
		Source:    "bus",
		CausedBy:  trigger.ID,
		Terminal:  true,
		Payload:   payload,
	}
	b.store.Append(ne)
}

// loopExhaustedPayload formats the diagnostic payload as a JSON object with
// the handler name and the cap that was hit. Kept as raw bytes to avoid an
// encoding/json import at the package level (used only here).
func loopExhaustedPayload(handlerName string, cap int) []byte {
	// Tiny ad-hoc JSON literal — handler names are alphanumeric/underscore
	// in practice, and the cap is an int, so escaping is unnecessary.
	return []byte(fmt.Sprintf(`{"handler":%q,"max_iterations":%d,"reason":"loop cap reached"}`,
		handlerName, cap))
}
