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
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
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

// Phase 4b control-plane event types — the bus emits these when its
// subscription table mutates. They describe the bus's own topology and are
// first-class events on the same log: audit handlers, the cycle detector,
// and downstream policy enforcers consume them. All five are terminal.
const (
	HandlerRegisteredType   = "HandlerRegistered"
	SubscribedType          = "Subscribed"
	UnsubscribedType        = "Unsubscribed"
	HandlerDeregisteredType = "HandlerDeregistered"
	SubscriptionRejectedType = "SubscriptionRejected"
)

// HandlerDescriptor is the static description of a registered handler. The
// bus keeps one per handler name in the live table and re-emits it on the
// HandlerRegistered control-plane event.
//
// MultiConsumes is the catch-all for handlers that subscribe to more than
// one event type (notably the audit handler reacting to the entire
// control-plane stream). When set, Consumes is informational; the live
// table records one subscription per type in MultiConsumes.
type HandlerDescriptor struct {
	Name          string
	Consumes      string
	MultiConsumes []string
	Emits         []EmittedDescriptor
	Description   string
}

// EmittedDescriptor mirrors handler.EmittedSpec on the wire of the
// HandlerRegistered control-plane event. Kept here to avoid a layering
// cycle between bus and handler.
type EmittedDescriptor struct {
	Type     string
	Terminal bool
	Optional bool
}

// SubscriptionInfo is one entry in the live subscription table. The cycle
// detector reads (handler, eventType) pairs to recompute the SCC.
type SubscriptionInfo struct {
	Handler   string
	EventType string
	// MaxIterations > 0 marks this subscription as part of a declared loop.
	// The cycle check treats any SCC touching a capped subscription as
	// allowable.
	MaxIterations int
}

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

	// Live control-plane table — Phase 4b. The dispatcher's fan-out still
	// reads `subscribers` (back-compat); the live table is the
	// authoritative model the cycle detector reads.
	descriptors   map[string]HandlerDescriptor // handler name → descriptor
	subscriptions []SubscriptionInfo           // ordered list of (handler, event_type) bindings

	// pendingControl is the FIFO of control-plane events emitted outside
	// any Run. Subsequent Runs prepend them to the dispatch queue so
	// subscribers (notably the audit handler) get a chance to react.
	pendingControl []event.Event
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
		store:       store,
		source:      "reflex",
		maxSteps:    256,
		clock:       func() time.Time { return time.Now().UTC() },
		descriptors: map[string]HandlerDescriptor{},
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

// Register adds sub to the bus. Subscribers fire in registration order when
// multiple match a single event. Safe to call concurrently — Phase 4a
// daemon mode invokes this from each accepted connection.
//
// Phase 4b: when sub implements Described (i.e. exposes its HandlerSpec),
// Register also records the descriptor in the live table and emits
// HandlerRegistered + Subscribed control-plane events. Subscribers without
// a descriptor (test fakes, ad-hoc adapters) are added silently as before.
func (b *Bus) Register(sub Subscriber) {
	b.subsMu.Lock()
	b.subscribers = append(b.subscribers, sub)
	desc, hasDesc := describeSub(sub)
	consumesList := []string{}
	if hasDesc {
		b.descriptors[desc.Name] = desc
		consumesList = subscriptionTypes(desc)
		cap := b.loopCaps[desc.Name]
		for _, et := range consumesList {
			b.subscriptions = append(b.subscriptions, SubscriptionInfo{
				Handler:       desc.Name,
				EventType:     et,
				MaxIterations: cap,
			})
		}
	}
	b.subsMu.Unlock()
	if hasDesc {
		b.emitHandlerRegistered(desc)
		cap := 0
		b.subsMu.RLock()
		cap = b.loopCaps[desc.Name]
		b.subsMu.RUnlock()
		for _, et := range consumesList {
			b.emitSubscribed(desc.Name, et, cap)
		}
	}
}

// subscriptionTypes returns the set of event types a handler subscribes
// to. MultiConsumes wins when set; otherwise Consumes is the single type;
// "*" / empty are treated as "no static subscription" — those handlers
// are dispatch-only (their Match() is responsible for choosing which
// events to react to, but the live table doesn't model an arbitrary
// match-anything binding).
func subscriptionTypes(desc HandlerDescriptor) []string {
	if len(desc.MultiConsumes) > 0 {
		out := make([]string, 0, len(desc.MultiConsumes))
		for _, t := range desc.MultiConsumes {
			if t != "" {
				out = append(out, t)
			}
		}
		return out
	}
	if desc.Consumes == "" || desc.Consumes == "*" {
		return nil
	}
	return []string{desc.Consumes}
}

// Described is the optional interface a Subscriber implements to opt into
// the Phase 4b control-plane table. The bus uses the descriptor for
// HandlerRegistered emission, the live-table cycle check, and the audit
// stream. Subscribers that don't implement it (test fakes) are tracked
// only as opaque dispatch targets — they will not appear in the audit log.
type Described interface {
	Descriptor() HandlerDescriptor
}

// describeSub extracts a HandlerDescriptor from a Subscriber via the
// Described interface. Returns ok=false when the Subscriber does not
// participate in the control-plane table (e.g. test fakes).
func describeSub(sub Subscriber) (HandlerDescriptor, bool) {
	if d, ok := sub.(Described); ok {
		desc := d.Descriptor()
		if desc.Name == "" {
			desc.Name = sub.Name()
		}
		return desc, true
	}
	return HandlerDescriptor{}, false
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

	// Phase 4b: drain any control-plane events queued outside Run before
	// firing the seed. The audit handler subscribes to these and would
	// otherwise miss the boot-time registrations.
	b.subsMu.Lock()
	pending := b.pendingControl
	b.pendingControl = nil
	b.subsMu.Unlock()

	first := b.Emit(seed)
	queue := append([]event.Event{}, pending...)
	queue = append(queue, first)
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
// another meta-event. Includes the Phase 4b control-plane events so a
// HandlerRegistered emission doesn't spawn an EventDispatched-of-control
// loop.
func isMetaEventType(typ string) bool {
	switch typ {
	case EventDispatchedType, DrainQuiescedType, HandlerFailedType, LoopExhaustedType,
		HandlerRegisteredType, SubscribedType, UnsubscribedType,
		HandlerDeregisteredType, SubscriptionRejectedType:
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

// LiveTable returns a snapshot of the current handler descriptors and the
// ordered subscription list. Used by the static-graph migration and the
// audit examples. The returned slices/maps are owned by the caller — the
// bus never mutates them after return.
func (b *Bus) LiveTable() (map[string]HandlerDescriptor, []SubscriptionInfo) {
	b.subsMu.RLock()
	defer b.subsMu.RUnlock()
	descs := make(map[string]HandlerDescriptor, len(b.descriptors))
	for k, v := range b.descriptors {
		descs[k] = v
	}
	subs := make([]SubscriptionInfo, len(b.subscriptions))
	copy(subs, b.subscriptions)
	return descs, subs
}

// Unsubscribe removes the binding (handler, eventType) from the live
// table. Idempotent — unsubscribing a non-existent binding is a no-op,
// not an error. Emits Unsubscribed.
//
// Phase 4b: this is a control-plane API. It does NOT remove the dispatch
// subscriber from `subscribers` — that requires HandlerDeregister, which
// pulls the subscriber from the dispatch list and emits the full set of
// removal events.
func (b *Bus) Unsubscribe(handlerName, eventType string) {
	b.subsMu.Lock()
	found := false
	out := b.subscriptions[:0]
	for _, s := range b.subscriptions {
		if s.Handler == handlerName && s.EventType == eventType {
			found = true
			continue
		}
		out = append(out, s)
	}
	b.subscriptions = out
	b.subsMu.Unlock()
	if found {
		b.emitUnsubscribed(handlerName, eventType)
	}
}

// HandlerDeregister removes the handler from the live table and the
// dispatch list, then emits Unsubscribed events for each removed
// subscription followed by a HandlerDeregistered. Idempotent.
func (b *Bus) HandlerDeregister(handlerName string) {
	b.subsMu.Lock()
	if _, ok := b.descriptors[handlerName]; !ok {
		b.subsMu.Unlock()
		return
	}
	delete(b.descriptors, handlerName)
	var removed []string
	keep := b.subscriptions[:0]
	for _, s := range b.subscriptions {
		if s.Handler == handlerName {
			removed = append(removed, s.EventType)
			continue
		}
		keep = append(keep, s)
	}
	b.subscriptions = keep
	// Pull the subscriber out of the dispatch list.
	subs := b.subscribers[:0]
	for _, s := range b.subscribers {
		if s.Name() == handlerName {
			continue
		}
		subs = append(subs, s)
	}
	b.subscribers = subs
	b.subsMu.Unlock()
	for _, et := range removed {
		b.emitUnsubscribed(handlerName, et)
	}
	b.emitHandlerDeregistered(handlerName)
}

// SubscribeWithCheck registers an additional (handler, eventType) binding
// against an already-registered handler. The cycle detector runs over the
// resulting live table; if the new edge would close an uncapped cycle the
// subscription is NOT recorded and a SubscriptionRejected control-plane
// event is emitted. On accept, a Subscribed event is emitted.
//
// This is the runtime authority promised by Phase 4b: subscriptions
// arriving over the wire (or from a config diff) get the same cycle check
// the YAML pre-flight does.
//
// Returns nil on accept, a non-nil error describing the rejection reason
// otherwise (the SubscriptionRejected event has already been emitted in
// that case).
func (b *Bus) SubscribeWithCheck(handlerName, eventType string, maxIterations int) error {
	b.subsMu.Lock()
	if _, ok := b.descriptors[handlerName]; !ok {
		b.subsMu.Unlock()
		reason := fmt.Sprintf("handler %q is not registered", handlerName)
		b.emitSubscriptionRejected(handlerName, eventType, reason)
		return fmt.Errorf("bus: %s", reason)
	}
	// Tentatively add and check.
	prev := append([]SubscriptionInfo(nil), b.subscriptions...)
	b.subscriptions = append(b.subscriptions, SubscriptionInfo{
		Handler:       handlerName,
		EventType:     eventType,
		MaxIterations: maxIterations,
	})
	cycle, ok := liveTableHasUncappedCycle(b.descriptors, b.subscriptions)
	if !ok {
		// Roll back.
		b.subscriptions = prev
		b.subsMu.Unlock()
		reason := fmt.Sprintf("would introduce uncapped cycle: %s", strings.Join(cycle, " -> "))
		b.emitSubscriptionRejected(handlerName, eventType, reason)
		return fmt.Errorf("bus: %s", reason)
	}
	if maxIterations > 0 {
		if b.loopCaps == nil {
			b.loopCaps = map[string]int{}
		}
		b.loopCaps[handlerName] = maxIterations
	}
	b.subsMu.Unlock()
	b.emitSubscribed(handlerName, eventType, maxIterations)
	return nil
}

// CheckLiveTableCycles runs the cycle detector over the current live table.
// Returns the SCC that breaks the cap rule, or (nil, true) when the table
// is acceptable. Used at startup after seed control-plane events have
// drained, and by tests.
func (b *Bus) CheckLiveTableCycles() ([]string, bool) {
	b.subsMu.RLock()
	defer b.subsMu.RUnlock()
	return liveTableHasUncappedCycle(b.descriptors, b.subscriptions)
}

// queueControl appends ev to the store and the pendingControl FIFO so a
// subsequent Run picks it up for fan-out.
func (b *Bus) queueControl(ev event.Event) {
	b.store.Append(ev)
	b.subsMu.Lock()
	b.pendingControl = append(b.pendingControl, ev)
	b.subsMu.Unlock()
}

func (b *Bus) emitHandlerRegistered(d HandlerDescriptor) {
	emits := make([]map[string]any, 0, len(d.Emits))
	for _, e := range d.Emits {
		em := map[string]any{"type": e.Type}
		if e.Terminal {
			em["terminal"] = true
		}
		if e.Optional {
			em["optional"] = true
		}
		emits = append(emits, em)
	}
	payload, _ := json.Marshal(map[string]any{
		"name":        d.Name,
		"consumes":    d.Consumes,
		"emits":       emits,
		"description": d.Description,
	})
	b.queueControl(event.Event{
		ID:       uuid.NewString(),
		Type:     HandlerRegisteredType,
		TS:       b.clock(),
		Source:   "bus",
		Terminal: true,
		Payload:  payload,
	})
}

func (b *Bus) emitSubscribed(handler, eventType string, maxIterations int) {
	body := map[string]any{
		"handler_name": handler,
		"event_type":   eventType,
	}
	if maxIterations > 0 {
		body["max_iterations"] = maxIterations
	}
	payload, _ := json.Marshal(body)
	b.queueControl(event.Event{
		ID:       uuid.NewString(),
		Type:     SubscribedType,
		TS:       b.clock(),
		Source:   "bus",
		Terminal: true,
		Payload:  payload,
	})
}

func (b *Bus) emitUnsubscribed(handler, eventType string) {
	payload, _ := json.Marshal(map[string]any{
		"handler_name": handler,
		"event_type":   eventType,
	})
	b.queueControl(event.Event{
		ID:       uuid.NewString(),
		Type:     UnsubscribedType,
		TS:       b.clock(),
		Source:   "bus",
		Terminal: true,
		Payload:  payload,
	})
}

func (b *Bus) emitHandlerDeregistered(handler string) {
	payload, _ := json.Marshal(map[string]any{"handler_name": handler})
	b.queueControl(event.Event{
		ID:       uuid.NewString(),
		Type:     HandlerDeregisteredType,
		TS:       b.clock(),
		Source:   "bus",
		Terminal: true,
		Payload:  payload,
	})
}

func (b *Bus) emitSubscriptionRejected(handler, eventType, reason string) {
	payload, _ := json.Marshal(map[string]any{
		"handler_name": handler,
		"event_type":   eventType,
		"reason":       reason,
	})
	b.queueControl(event.Event{
		ID:       uuid.NewString(),
		Type:     SubscriptionRejectedType,
		TS:       b.clock(),
		Source:   "bus",
		Terminal: true,
		Payload:  payload,
	})
}

// liveTableHasUncappedCycle runs Tarjan's SCC over the live subscription
// table. For every (handler H, event E) binding in `subs`, an edge exists
// from any handler whose descriptor lists E in Emits to H. An SCC is
// acceptable when at least one node in the SCC has MaxIterations > 0 on
// the subscription that's part of the cycle.
//
// Returns (offending-scc, ok). ok=true means no uncapped cycle.
func liveTableHasUncappedCycle(descs map[string]HandlerDescriptor, subs []SubscriptionInfo) ([]string, bool) {
	// Build adjacency: from = emitter handler name, to = subscribed handler name.
	// Also record the per-handler "cap" — true if any of this handler's
	// subscriptions has MaxIterations > 0, OR (back-compat) if loopCaps
	// records this handler.
	capped := map[string]bool{}
	for _, s := range subs {
		if s.MaxIterations > 0 {
			capped[s.Handler] = true
		}
	}
	adj := map[string][]string{}
	for name := range descs {
		adj[name] = nil
	}
	for _, s := range subs {
		consumer := s.Handler
		// For each handler whose descriptor emits s.EventType, add an edge
		// emitter → consumer. Skip terminal emissions — a terminal cannot
		// spawn descendants, so it can't close a runtime cycle.
		for emitterName, d := range descs {
			for _, em := range d.Emits {
				if em.Type != s.EventType {
					continue
				}
				if em.Terminal {
					continue
				}
				adj[emitterName] = append(adj[emitterName], consumer)
			}
		}
	}
	// Tarjan
	index := 0
	indexOf := map[string]int{}
	lowlink := map[string]int{}
	onStack := map[string]bool{}
	stack := []string{}
	var sccs [][]string

	var strongConnect func(v string)
	strongConnect = func(v string) {
		indexOf[v] = index
		lowlink[v] = index
		index++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range adj[v] {
			if _, seen := indexOf[w]; !seen {
				strongConnect(w)
				if lowlink[w] < lowlink[v] {
					lowlink[v] = lowlink[w]
				}
			} else if onStack[w] {
				if indexOf[w] < lowlink[v] {
					lowlink[v] = indexOf[w]
				}
			}
		}
		if lowlink[v] == indexOf[v] {
			var scc []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				scc = append(scc, w)
				if w == v {
					break
				}
			}
			// Non-trivial only (size > 1 OR self-loop).
			if len(scc) > 1 || hasSelfLoopName(scc[0], adj) {
				sccs = append(sccs, scc)
			}
		}
	}
	var names []string
	for n := range adj {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, v := range names {
		if _, seen := indexOf[v]; !seen {
			strongConnect(v)
		}
	}
	for _, scc := range sccs {
		// Any capped node in the SCC absolves it.
		anyCapped := false
		for _, n := range scc {
			if capped[n] {
				anyCapped = true
				break
			}
		}
		if !anyCapped {
			sort.Strings(scc)
			return scc, false
		}
	}
	return nil, true
}

func hasSelfLoopName(v string, adj map[string][]string) bool {
	for _, w := range adj[v] {
		if w == v {
			return true
		}
	}
	return false
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
