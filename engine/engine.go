package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
)

// Engine is the kernel: one append-only log, one dispatcher. Its whole
// public surface is the four methods below — append (the sole write),
// apply (the changeset pipeline), drain (dispatch to quiescence), and
// the readable log (no privileged plane, G8: audit, cost, every metric
// is a fold over Events).
type Engine struct {
	log []Event
	// bodies maps a node name → its Reaction. It is the ONE part of the topology
	// that is not a fact on the log: a Body is Go code, resolved by name from a
	// process-level registry exactly as view-type builders (RegisterType) and
	// provider adapters are code, not facts (changeset.go package doc). Apply
	// commits a node's body here on a successful apply; foldTopology reattaches
	// it by name. The wiring itself is fully recomputable from the log (G8).
	bodies map[string]Reaction
	// resolver builds descriptor-based node bodies (BodyKind + BodyConfig) into
	// Reactions (resolver.go). nil for the in-process path (live Body closures);
	// the daemon/YAML path installs one via WithBodyResolver.
	resolver BodyResolver
	// live is the cached live topology — a memoisation of foldTopology(log,
	// bodies), nothing more (G8: it is recomputed from the log, never the truth).
	// Apply rebuilds it after writing the changeset's facts; liveDecls lazily
	// builds it on first read so a fresh engine reports an empty topology.
	live []Decl
	// frontier is the index of the next undispatched event in log. Drain
	// advances it as it dispatches; re-running Drain resumes from here, so a
	// crash mid-drain recovers by simply calling Drain again (G5).
	frontier int
	// dispatched[i] records that log[i] was driven by process (dispatched to
	// its subscribers and its caused subtree). Depth-first dispatch appends a
	// reaction's children at the END of the log and drives them immediately, so
	// the dispatched set is NOT a contiguous prefix when several ingress events
	// are appended before one Drain (each ingress roots its own cone; the first
	// cone's children sit past the later ingress roots). Tracking dispatch
	// per-index — instead of a single high-water frontier — lets Drain still
	// reach every un-driven ingress root, so N parallel request cones in one
	// Drain all run (doc 26 §2a isolation needs N cones to exist). It is a fold
	// over the log (rebuildScopes recomputes it), not a privileged store.
	dispatched []bool
}

// New returns an empty engine: no topology, no events. Everything it
// will ever hold arrives through Apply and Append. Options configure the
// engine (e.g. WithBodyResolver for the declarative/daemon path).
func New(opts ...Option) *Engine {
	e := &Engine{bodies: map[string]Reaction{}}
	for _, o := range opts {
		o(e)
	}
	return e
}

// Load reconstructs an engine from a persisted or replayed log (the daemon's
// crash-recovery / restart path): it adopts the log and rebuilds every
// descriptor-based node body from the recorded sys.subscriber.registered facts via the
// resolver. This is the proof of G8 for behaviour wiring — only the body kind +
// config are facts; the resolver rebuilds the code. A descriptor whose body
// cannot be resolved is a fatal load error (the daemon refuses to start on a log
// it cannot fully reconstitute). Load does NOT drain; the caller drives recovery
// by calling Drain (rebuildScopes re-derives any in-flight cones).
func Load(log []Event, opts ...Option) (*Engine, error) {
	e := New(opts...)
	e.log = log
	for _, ev := range log {
		if ev.Subject != subjSubscriberRegistered {
			continue
		}
		var s subscriberSpec
		if json.Unmarshal(ev.Payload, &s) != nil || s.Name == "" || s.BodyKind == "" {
			continue
		}
		r, err := e.resolveBody(s.Name, s.BodyKind, s.BodyConfig)
		if err != nil {
			return nil, fmt.Errorf("engine: load: node %q: %w", s.Name, err)
		}
		e.bodies[s.Name] = r
	}
	e.live = foldTopology(e.log, e.bodies)
	return e, nil
}

// resolveBody builds one descriptor body through the installed resolver (or
// reports that none is installed — a descriptor node with no resolver is a
// configuration error, surfaced as a rejected changeset / failed load).
func (e *Engine) resolveBody(name, kind string, config json.RawMessage) (Reaction, error) {
	if e.resolver == nil {
		return nil, fmt.Errorf("node %q declares body kind %q but no body resolver is installed (pass engine.WithBodyResolver)", name, kind)
	}
	return e.resolver(name, kind, config)
}

// Apply runs the changeset pipeline (doc 20 / CONCEPT §8): it is the in-process
// client of the changeset grammar. The resulting graph — the current live table
// folded from the log PLUS the requested decls — is validated as a whole (not
// each step), and on success the engine writes the facts the live table folds.
//
// The grammar is on the log: a sys.topology.changeset.requested fact records
// the ops; on a connected resulting graph the engine appends one object fact per
// decl (sys.subscriber.registered, sys.scope.declared, …) followed by
// sys.topology.changeset.applied, and commits the nodes' bodies to the registry;
// on a disconnected graph it appends sys.topology.changeset.rejected with the
// reasons and writes no object facts (so the live table is unchanged by
// construction) and returns a ValidationError carrying the Report. Validate
// remains the cleaner read-only dry-run surface; Apply is the writing client.
//
// No drain runs here (doc 20: "appended as one batch between dispatches"): the
// facts are dispatched on the next Drain, so an audit subscriber of the sys.*
// kinds sees them like any other event.
func (e *Engine) Apply(ctx context.Context, decls ...Decl) error {
	live := e.liveDecls()
	resulting := make([]Decl, 0, len(live)+len(decls))
	resulting = append(resulting, live...)
	resulting = append(resulting, decls...)

	rep, err := Validate(resulting...)
	if err != nil {
		return err
	}

	// Resolve every descriptor-based body up front (a body that cannot be built
	// is a rejection, never a silent nil): a live Body is taken as-is, a
	// BodyKind descriptor is run through the resolver. Failures join the
	// connectivity gaps in the single reject reason list.
	resolved := map[string]Reaction{}
	var bodyErrs []string
	for _, d := range decls {
		n, ok := d.(Subscriber)
		if !ok {
			continue
		}
		if n.Body != nil {
			resolved[n.Name] = n.Body
			continue
		}
		if subscriberBodyDescriptor(n) {
			r, rerr := e.resolveBody(n.Name, n.BodyKind, n.BodyConfig)
			if rerr != nil {
				bodyErrs = append(bodyErrs, rerr.Error())
				continue
			}
			resolved[n.Name] = r
		}
	}

	req := e.appendSys("", SubjChangesetRequested, mustMarshal(changesetPayload{
		Ops:       opsOf(decls),
		Principal: "apply",
	}))

	if !rep.Connected || len(bodyErrs) > 0 {
		e.appendSys(req.Trace.SpanID, SubjChangesetRejected, mustMarshal(rejectedPayload{
			Changeset: req.Trace.SpanID,
			Reasons:   append(append([]string(nil), rep.Suggestions...), bodyErrs...),
		}))
		if !rep.Connected {
			return &ValidationError{Report: rep}
		}
		return fmt.Errorf("engine: changeset rejected — %d node body(ies) failed to resolve: %v", len(bodyErrs), bodyErrs)
	}

	for _, op := range opsOf(decls) {
		if subject, payload := factOf(op); subject != "" {
			e.appendSys(req.Trace.SpanID, subject, payload)
		}
	}
	for name, r := range resolved {
		e.bodies[name] = r
	}
	e.appendSys(req.Trace.SpanID, SubjChangesetApplied, mustMarshal(appliedPayload{
		Changeset: req.Trace.SpanID,
		Count:     len(decls),
	}))

	// The live table is exactly the fold of the facts just written (G8): rebuild
	// the memoisation from the log so the hot dispatch paths read it in O(1).
	e.live = foldTopology(e.log, e.bodies)
	return nil
}

// liveDecls returns the cached live topology, lazily folding it from the log on
// first read (a fresh engine has no facts → an empty topology). The cache is a
// pure memoisation of foldTopology(log, bodies); Apply rebuilds it whenever the
// topology facts change. Dynamic event.registered facts emitted DURING a drain
// do not change the wiring, so they need no cache rebuild — the catalog path
// folds them straight from the log (see catalogSchema).
func (e *Engine) liveDecls() []Decl {
	if e.live == nil {
		e.live = foldTopology(e.log, e.bodies)
	}
	return e.live
}

// install records a topology directly into the live-table cache, bypassing the
// changeset pipeline and its connectivity validation. It is a TEST seam:
// white-box engine tests construct deliberately-disconnected topologies (no
// ingress root, intentional dead-ends, raw cycles) to exercise the dispatcher,
// scope, and projection mechanics in isolation — graphs Apply's validator is
// built to reject. Production code goes through Apply, where the live table is a
// genuine fold of the changeset facts (G8). install commits the nodes' bodies
// and appends to the cache, the same shape a fold would produce.
func (e *Engine) install(decls ...Decl) {
	for _, d := range decls {
		if n, ok := d.(Subscriber); ok && n.Body != nil {
			e.bodies[n.Name] = n.Body
		}
	}
	e.live = append(e.liveDecls(), decls...)
}

// Topology returns a snapshot of the live topology — the fold of the changeset
// facts (doc 20 "reads": the table behind `reflex topo show`). It is a copy, so
// a caller cannot mutate the engine's cache.
func (e *Engine) Topology() []Decl {
	live := e.liveDecls()
	return append([]Decl(nil), live...)
}

// appendSys appends an engine-written sys fact to the log (the control plane's
// uprightness rule: only the engine writes facts). It mints a span, links the
// optional cause (the changeset.requested event the facts hang under), and
// carries no session/request — sys-class facts are session-less machinery.
func (e *Engine) appendSys(cause, subject string, payload json.RawMessage) Event {
	var causedBy []string
	if cause != "" {
		causedBy = []string{cause}
	}
	ev := Event{
		Subject: subject,
		Payload: payload,
		Trace: Trace{
			SpanID:   e.mintSpan(),
			CausedBy: causedBy,
		},
	}
	e.log = append(e.log, ev)
	return ev
}

// ValidationError reports a topology that failed connectivity validation
// (doc 27 §5). It carries the full Report so the caller can render the gaps
// and the suggested LLM bridges.
type ValidationError struct {
	Report Report
}

func (e *ValidationError) Error() string {
	return "engine: topology is not connected — " +
		"dead-ends, unreachable nodes, fragments, or unbounded cycles present (see Report)"
}

// Append puts one event on the log — the sole write (§4) — and stamps
// it: span id minted, session resolved, request id derived. Adapters and
// the operator surface call this for ingress; reactions never do (their
// Emits are appended by the dispatcher).
func (e *Engine) Append(_ context.Context, subject string, payload json.RawMessage) (Event, error) {
	ev := Event{
		Subject: subject,
		Payload: payload,
		Trace: Trace{
			SpanID:    e.mintSpan(),
			SessionID: sessionOf(subject),
			// request_id is the narrowest request-scope covering all causes
			// (§2), derived in Drain's dispatch once cone membership is known
			// (see process). An externally-appended ingress event has no cause
			// and is in no request cone yet, so it is empty here — correct, not
			// pending.
			RequestID: "",
			// CausedBy is empty: an externally-appended event is ingress, it has
			// no cause inside the log (§2 uprightness — causality is a fact of
			// the log, and an ingress event roots its own chain).
			CausedBy: nil,
		},
	}
	e.log = append(e.log, ev)
	return ev, nil
}

// mintSpan returns a deterministic, replay-stable span id: a monotonic
// counter over the current log length (§2: event ≡ span). No time, no
// randomness — the same scenario replayed yields the same ids, which is what
// the acceptance test pins.
func (e *Engine) mintSpan() string {
	return fmt.Sprintf("e%d", len(e.log))
}

// Drain dispatches until every open obligation closes (§5): events fan
// out to the live table with correlation stamped and budgets enforced,
// scope closures fire exactly once per instance, and the call returns at
// quiescence. Crash recovery is re-running Drain — the scope state is a fold
// over the log, recomputed at entry (G5/G8).
//
// Obligation accounting is hierarchical, so dispatch is depth-first (doc 24
// §5): an event entering a cone increments it, its children increment their
// cones BEFORE the parent decrements, and an instance whose count crosses to
// zero closes exactly once (G6). The flat 2a frontier loop only drives the
// roots of that recursion — events at the frontier with no open cone above
// them (ingress) — and a re-drive resumes from the recomputed frontier.
func (e *Engine) Drain(ctx context.Context) error {
	nodes := e.liveSubscribers()
	sr := e.rebuildScopes(nodes)

	// Drive each undispatched event depth-first to quiescence. process dispatches
	// the event AND its whole caused subtree (marking each dispatched), appending
	// as it goes; the loop's cursor skips events process already drove. The
	// cursor scans the whole log — not a contiguous frontier — so a later ingress
	// root that an earlier cone's depth-first dispatch jumped over is still
	// reached: N parallel cones in one Drain all run. Quiescence is every event
	// dispatched with no open obligation.
	for idx := 0; idx < len(e.log); idx++ {
		if e.isDispatched(idx) {
			continue
		}
		e.process(ctx, idx, nodes, sr)
	}
	return nil
}

// isDispatched reports whether log[idx] has been driven by process. The
// dispatched slice grows lazily with the log; an index past its end is
// undispatched.
func (e *Engine) isDispatched(idx int) bool {
	return idx < len(e.dispatched) && e.dispatched[idx]
}

// markDispatched records that log[idx] was driven by process, growing the
// dispatched slice to cover it. It also advances frontier to the high-water
// mark so rebuildScopes (which replays the dispatched prefix) sees every driven
// event on a re-drive.
func (e *Engine) markDispatched(idx int) {
	for len(e.dispatched) <= idx {
		e.dispatched = append(e.dispatched, false)
	}
	e.dispatched[idx] = true
	if e.frontier <= idx {
		e.frontier = idx + 1
	}
}

// rebuildScopes reconstructs the scope-runtime cache from the log up to the
// frontier (G8: in-memory state is a fold over Events). Every event already
// dispatched has its membership and the instances it rooted recomputed, and
// already-emitted scope.{name}.closed / .budget_exhausted facts mark their
// instances closed/exhausted so a re-drive never re-emits them (G6). Counts
// and obligations of still-open instances are rebuilt so the depth-first pass
// resumes exactly.
func (e *Engine) rebuildScopes(nodes []Subscriber) *scopeRuntime {
	sr := newScopeRuntime(declaredScopes(e.liveDecls()), nodes)
	// Replay DISPATCHED events in log order, re-rooting and re-stamping
	// membership. Obligations are not replayed (they are an in-flight quantity of
	// a drain in progress); a fresh drain re-derives them as it dispatches. Closed
	// instances are marked so closure is not re-emitted. Un-dispatched ingress
	// roots interleaved below the high-water frontier are skipped — they have no
	// dispatched descendants yet, so they contribute nothing to the resumed fold.
	for i := 0; i < e.frontier && i < len(e.log); i++ {
		if !e.isDispatched(i) {
			continue
		}
		ev := e.log[i]
		_, scope, kind := splitSubject(ev.Subject)
		sr.admit(ev, scope, kind)
		// A scope.{name}.closed already on the log seals its instance (caused by
		// the instance root): re-mark so a re-drive never re-emits closure (G6).
		if name, reason, _, ok := parseScopeFact(kind); ok && reason == "closed" {
			for _, cause := range ev.Trace.CausedBy {
				if inst := sr.instances[instanceKey(name, cause)]; inst != nil {
					inst.closed = true
				}
			}
		}
	}
	return sr
}

// process dispatches one event depth-first, maintaining the obligation count
// (doc 24 §5). It enters the event into its cones (+1 each), runs the matched
// reactions appending their emits, recursively processes each emitted child
// (so the child's increments land BEFORE this event's decrement — the
// false-zero guard), and finally leaves the cones (−1 each), closing any whose
// count crossed to zero.
func (e *Engine) process(ctx context.Context, idx int, nodes []Subscriber, sr *scopeRuntime) {
	e.markDispatched(idx)
	ev := e.log[idx]
	cls, scope, kind := splitSubject(ev.Subject)

	// Budget cap FIRST, on the cones the event inherits by causality (doc 26
	// §3d): if a budgeted parent cone has already reached its ceiling for this
	// kind, the event is INERT — it is on the log (it happened, doc 26 §3e) but
	// the engine does not admit it into the cone at all: it does not root a new
	// scope, does not count toward obligations, and is delivered to no
	// subscriber. Only the graceful budget_exhausted fact fires, once. Rooting
	// before the gate would let a starved loop-trigger open an empty child cone
	// that closes and re-drives the loop forever; the cap must precede rooting.
	if exhausted := sr.gateInherited(ev, kind); exhausted != nil {
		sr.membership[ev.Trace.SpanID] = nil // inert: in no cone
		e.emitScopeFact(ctx, exhausted, "budget_exhausted", kind, sr)
		return
	}

	// Membership + rooting: an event roots its instances and joins its parents'
	// cones before it is counted, so the +1 lands on every covering instance
	// including ones it just opened. admit also records the dispatched count of
	// the bounded kind in each cone (consumed by the gate on the next event).
	sr.admit(ev, scope, kind)
	sr.tallyKind(ev.Trace.SpanID, kind)

	// Stamp request_id now that membership is known (doc 24 §2: narrowest
	// request scope covering all causes). Re-stamp the stored event too so the
	// log carries it.
	if req := sr.narrowestRequest(ev.Trace.SpanID); req != "" {
		ev.Trace.RequestID = req
		e.log[idx].Trace.RequestID = req
	}

	sr.enter(ev.Trace.SpanID)
	e.fanOut(ctx, idx, cls, scope, kind, nodes, sr)

	// Leave the cones; any that reach zero quiesce and close exactly once.
	for _, key := range sr.membership[ev.Trace.SpanID] {
		if sr.leave(key) {
			e.emitClosure(ctx, sr.instances[key], sr)
		}
	}
}

// fanOut runs every matching live node and recursively processes each emit as
// a child of ev. A React error is not fatal (§4 G3): it becomes a non-terminal
// {node}.failed event in the same cone and the drain continues.
func (e *Engine) fanOut(ctx context.Context, idx int, cls, scope, kind string, nodes []Subscriber, sr *scopeRuntime) {
	ev := e.log[idx]
	for _, n := range nodes {
		if !sr.deliver(n, ev.Trace.SpanID, scope, kind) {
			continue
		}
		if n.Body == nil {
			// A sink consumes its trigger and emits nothing (notify, lifecycle,
			// a barrier). The obligation it held is released when its trigger
			// leaves the cone — there is no reaction to run.
			continue
		}
		// Attach the real Views (doc 24 §6): the node's declared projections,
		// each evaluated at THIS trigger's causal position (the backward
		// caused_by walk to the projection's horizon). The evaluator indexes the
		// log as it stands now, so the view sees exactly the trigger's causal
		// past — "in context" ≡ "in the causal past" (reaction.go Views doc).
		catLookup := e.catalogLookup()
		views := Views(emptyViews{schema: catLookup})
		if len(n.Reads) > 0 {
			views = projectionViews{
				eval:        newProjectionEval(e.log, e.liveDecls(), sr),
				triggerSpan: ev.Trace.SpanID,
				schema:      catLookup,
			}
		}
		emits, err := n.Body.React(ctx, ev, views)
		if err != nil {
			p, _ := json.Marshal(map[string]string{"error": err.Error()})
			child := e.appendEmit(ev, cls, Emit{Kind: n.Name + ".failed", Payload: p})
			e.process(ctx, child, nodes, sr)
			continue
		}
		for _, em := range emits {
			// Payload-conformance (doc 26 §4a runtime half): this is the ONE place
			// the engine reads an emit's payload, and only to validate it against
			// the emit kind's declared catalog schema — it never interprets
			// meaning. The catalog is re-folded from the log as it stands (decls +
			// event.registered facts appended so far this drain), so a dynamic
			// registration earlier in the drain is in scope; folding it here keeps
			// the engine payload-blind everywhere else and replay-stable (the fold
			// is deterministic over the log). OPT-IN: a kind not in the catalog is
			// skipped (the type layer is dormant until adopted, doc 26 §4a). A
			// non-conforming payload becomes the BODY'S OWN .failed (an event, not a
			// crash — G3 / doc 26 §4), and the drain continues.
			if schema, known := e.catalogSchema(em.Kind); known {
				if err := conforms(em.Payload, schema); err != nil {
					p, _ := json.Marshal(map[string]string{
						"error": fmt.Sprintf("payload does not conform to schema for kind %q: %v", em.Kind, err),
						"kind":  em.Kind,
					})
					child := e.appendEmit(ev, cls, Emit{Kind: n.Name + ".failed", Payload: p})
					e.process(ctx, child, nodes, sr)
					continue
				}
			}
			child := e.appendEmit(ev, cls, em)
			e.process(ctx, child, nodes, sr)
		}
	}
}

// appendEmit places an emit on the log as a caused event and returns its log
// index. The dispatcher owns scope placement (§2 uprightness): the new subject
// is the emit's kind under the triggering event's class+scope, never a scope
// the reaction chose. Trace inherits the session, mints a fresh span, and
// records the trigger as the single cause. process drives the child directly
// (depth-first) and marks it dispatched, so the Drain cursor skips it.
func (e *Engine) appendEmit(trigger Event, cls string, em Emit) int {
	idx := len(e.log)
	ev := Event{
		Subject: placeSubject(cls, em.Kind),
		Payload: em.Payload,
		Trace: Trace{
			SpanID:    e.mintSpan(),
			SessionID: trigger.Trace.SessionID,
			RequestID: trigger.Trace.RequestID, // narrowed in process once membership is known
			CausedBy:  []string{trigger.Trace.SpanID},
		},
	}
	e.log = append(e.log, ev)
	return idx
}

// emitClosure drops scope.{name}.closed for a quiesced instance exactly once
// (G6), placed in the cone's parent (doc 26 §2 sealing). The closed event
// carries the instance id so a join/barrier/bridge consumer can correlate, and
// it is itself dispatched depth-first so its parent-cone consumers run within
// the same drain.
func (e *Engine) emitClosure(ctx context.Context, inst *scopeInstance, sr *scopeRuntime) {
	e.emitScopeFact(ctx, inst, "closed", "", sr)
}

// emitScopeFact appends a scope.{name}.{kind} fact (closed | budget_exhausted)
// caused by the instance's root, placed in the cone's parent class+scope so
// the membership rule keeps it outside the just-sealed cone, and drives it
// depth-first so its consumers fire in this drain. The instance id rides in
// the payload (engine stays payload-blind on the way out; consumers read it).
// session/request_id are left for process to stamp from the recomputed
// membership — the fact lands in the parent cone by the sealing rule, so its
// request_id is the parent's, derived not guessed.
func (e *Engine) emitScopeFact(ctx context.Context, inst *scopeInstance, reason, kind string, sr *scopeRuntime) {
	idx := len(e.log)
	subjectKind := "scope." + inst.name + "." + reason
	// Snapshot the closing instance's final per-scope state (doc 26 §2a): the
	// engine's own maintained fold of the cone's state.updated.{path} events,
	// frozen at quiescence and carried in the closure payload so a parent-scope
	// consumer can promote chosen fields up. Computed over the cone as it stands
	// now (the closure is appended just below, so the snapshot excludes it —
	// correct: the closed fact is not part of the state it seals). Payload-blind:
	// path → payload bytes.
	pe := newProjectionEval(e.log, e.liveDecls(), sr)
	state := pe.scopeState(inst.name, inst.rootSpan)
	ev := Event{
		Subject: placeSubject(inst.placeClass, subjectKind),
		Payload: mustMarshal(closedPayload{
			Instance: inst.rootSpan,
			Scope:    inst.name,
			Reason:   reason,
			Kind:     kind,
			State:    state,
		}),
		Trace: Trace{
			SpanID:    e.mintSpan(),
			SessionID: inst.placeSession,
			CausedBy:  []string{inst.rootSpan},
		},
	}
	e.log = append(e.log, ev)
	e.process(ctx, idx, sr.nodes, sr)
}

// catalogSchema returns the declared catalog schema for a kind and whether the
// kind is in the catalog at all (doc 26 §4a). It folds the catalog from the
// recorded decls (EventKind bootstrap form) plus the log so far (event.registered
// dynamic form) — recomputable from the log (G8), no privileged store. A known
// kind with no declared schema returns (nil, true): known, no payload constraint
// (conforms treats a nil schema as conforming). An unknown kind returns
// (nil, false), and the caller skips the conformance check (opt-in dormancy).
func (e *Engine) catalogSchema(kind string) (json.RawMessage, bool) {
	cat := foldCatalog(e.liveDecls(), e.log)
	return cat.schemaOf(kind)
}

// catalogLookup folds the catalog once and returns a per-dispatch schema lookup
// for the read surface (Views.Schema) — so a body can advertise the schemas of
// the kinds it may emit without each call re-folding the catalog.
func (e *Engine) catalogLookup() schemaFunc {
	cat := foldCatalog(e.liveDecls(), e.log)
	return cat.schemaOf
}

// liveSubscribers returns the reaction nodes from the recorded decls, with scope
// defaulting applied (empty In → "global", matching Validate's foldSubscribers). The
// live table Drain dispatches against is exactly the validated topology Apply
// stored.
func (e *Engine) liveSubscribers() []Subscriber {
	return foldSubscribers(e.liveDecls())
}

// subscriberMatches reports whether a node subscribes to an event with the given
// scope token and kind. A node matches iff (a) its scope qualifier admits the
// event's scope and (b) some On pattern matches the kind after handler desugar
// (§2: the node binds the kind, the bus prepended the scope wildcard, so we
// match On against the kind tail alone).
func subscriberMatches(n Subscriber, scope, kind string) bool {
	if !scopeAdmits(n.In, scope) {
		return false
	}
	for _, pat := range n.On {
		if subjectMatch(pat, kind) {
			return true
		}
	}
	return false
}

// scopeAdmits applies the node's In qualifier (§5). "global" (the default)
// admits any scope; otherwise the event's scope token must equal In.
//
// 2a simplification: scope is matched as a single equality on the scope token
// (e.g. In == "session"), not the full per-instance cone membership of §5 —
// instances and cone delivery are stage 2b. A session-class event carries
// scope token "session"; sys/ingress events carry "" and are admitted only by
// a "global" node.
func scopeAdmits(in, scope string) bool {
	if in == "" || in == "global" {
		return true
	}
	return in == scope
}

// emptyViews is the 2a read surface: no projections exist yet, so every view
// is empty. Stage 2b/§6 attaches the declared projections evaluated at the
// trigger's causal position.
type emptyViews struct{ schema schemaFunc }

func (emptyViews) Value(string) any   { return nil }
func (emptyViews) KV(string) KV       { return emptyKV{} }
func (emptyViews) Log(string) []Event { return nil }
func (v emptyViews) Schema(kind string) (json.RawMessage, bool) {
	if v.schema == nil {
		return nil, false
	}
	return v.schema(kind)
}

type emptyKV struct{}

func (emptyKV) Get(string) (json.RawMessage, bool) { return nil, false }
func (emptyKV) Keys() []string                     { return nil }

// Events is the log, readable in order. Every consumer that is not a
// wired Reaction — audit, cost accounting, the trace workbench, tests —
// is a fold over this sequence; none of them get a privileged surface.
func (e *Engine) Events() iter.Seq[Event] {
	return func(yield func(Event) bool) {
		for _, ev := range e.log {
			if !yield(ev) {
				return
			}
		}
	}
}
