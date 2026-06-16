package engine

import (
	"encoding/json"
	"fmt"
)

// The control plane (doc 20 / CONCEPT §8): managing the topology — adding or
// removing nodes, subscriptions, scopes, projections, and event types — is
// itself events on the log, not a config edit + restart. A client never mutates
// the live table; it REQUESTS a changeset, the engine validates the resulting
// graph as a whole, and on success the engine writes the facts the live table
// folds:
//
//	sys.topology.changeset.requested{ ops, principal }
//	   → engine validates fold(live table) + ops → resulting graph
//	   → facts:  sys.subscriber.registered · sys.scope.declared · sys.projection.registered
//	             · sys.event.registered (+ the .deregistered/.retired removals)
//	            + sys.topology.changeset.applied{ id }
//	   | sys.topology.changeset.rejected{ id, reasons }
//
// The live table folds ONLY facts, and only the engine writes facts (the
// control plane's uprightness rule, the exact parallel of caused_by being
// dispatcher-stamped): a client cannot claim a node into existence around the
// validator, because the fact kinds are engine-written. The wiring is fully
// recomputable from the log (G8) via foldTopology; the one non-serializable
// part — a Node's Body, which is Go code — is resolved by name from a
// process-level registry, exactly as view-type builders and provider adapters
// are code resolved by name rather than facts on the log.
//
// Apply is the in-process client + engine handler fused: it derives the ops
// from the Decls the caller passes, stashes their bodies, and runs the pipeline.
// A future daemon/CLI is another client of the same grammar, differing only by
// principal (doc 20 "one pipeline, every source of change").

// Changeset fact subjects (doc 20 "the managed surface"). All are sys-class
// (session-less machinery): the kind tail is what foldTopology switches on. The
// requested/applied/rejected trio is exported so a daemon/CLI client and a
// --wait predicate can name them; the per-object fact subjects are internal —
// they are written only by the engine.
const (
	// SubjChangesetRequested carries the requested ops + principal — the
	// grammar's first event, appended whether the changeset is accepted or not.
	SubjChangesetRequested = "sys.topology.changeset.requested"
	// SubjChangesetApplied seals an accepted changeset (after its facts).
	SubjChangesetApplied = "sys.topology.changeset.applied"
	// SubjChangesetRejected records a refused changeset with its reasons; no
	// object facts are written, so the live table is unchanged by construction.
	SubjChangesetRejected = "sys.topology.changeset.rejected"

	// KindChangesetRequested/Applied/Rejected are the KIND TAILS of the three
	// changeset facts (the Subj* constants above are the full sys subjects;
	// splitSubject strips the "sys." class). A node drives the control plane
	// in-graph by emitting KindChangesetRequested as an ordinary event, and hears
	// the outcome on KindChangesetApplied / KindChangesetRejected. The engine
	// self-registers all three in the catalog (catalog.go), terminal.
	KindChangesetRequested = "topology.changeset.requested"
	KindChangesetApplied   = "topology.changeset.applied"
	KindChangesetRejected  = "topology.changeset.rejected"

	subjSubscriberRegistered   = "sys.subscriber.registered"
	subjSubscriberDeregistered = "sys.subscriber.deregistered"
	subjScopeDeclared          = "sys.scope.declared"
	subjScopeRetired           = "sys.scope.retired"
	subjProjectionRegistered   = "sys.projection.registered"
	subjProjectionDeregistered = "sys.projection.deregistered"
	// subjEventRegistered is the catalog's registration fact placed in the sys
	// class; its kind tail is exactly the catalog seed kind "event.registered"
	// (catalog.go), so foldCatalog folds it natively AND foldTopology
	// reconstructs the EventKind decl from the same fact — one fact, both folds.
	subjEventRegistered = "sys.event.registered"
)

// Op is one managed-object mutation in a changeset (doc 20): a verb over an
// object kind, identified by name, carrying the serializable spec (nil for a
// removal). It is the public unit a daemon/CLI emits; Apply derives ops from
// Decls. Spec is the wiring only — a Node's Body never appears here (bodies are
// code, resolved by name, see changeset.go's package doc).
type Op struct {
	Verb string          `json:"verb"` // "add" | "remove"
	Kind string          `json:"kind"` // "subscriber" | "scope" | "projection" | "event"
	Name string          `json:"name"`
	Spec json.RawMessage `json:"spec,omitempty"`
}

const (
	verbAdd    = "add"
	verbRemove = "remove"

	opKindSubscriber = "subscriber"
	opKindScope      = "scope"
	opKindProjection = "projection"
	opKindEvent      = "event"
)

// changesetPayload is the sys.topology.changeset.requested payload: the ops and
// the requesting principal (doc 20). The principal feeds a permission check (a
// later knob); in-process Apply uses a fixed principal.
type changesetPayload struct {
	Ops       []Op   `json:"ops"`
	Principal string `json:"principal,omitempty"`
}

// appliedPayload / rejectedPayload are the trailing facts of the grammar. The
// changeset id is the requested event's span id (the cone the facts hang under).
type appliedPayload struct {
	Changeset string `json:"changeset"`
	Count     int    `json:"count"`
}

type rejectedPayload struct {
	Changeset string   `json:"changeset"`
	Reasons   []string `json:"reasons,omitempty"`
}

// subscriberSpec is the serializable wiring of a Node — everything but Body (doc 20 /
// changeset.go package doc). The Body is reattached from the engine's registry
// by Name at fold time.
type subscriberSpec struct {
	Name  string   `json:"name"`
	On    []string `json:"on,omitempty"`
	In    string   `json:"in,omitempty"`
	Reads []string `json:"reads,omitempty"`
	Emits []string `json:"emits,omitempty"`
	Scope string   `json:"scope,omitempty"`
	// BodyKind + BodyConfig are the serializable body descriptor (resolver.go).
	// A live in-process Body is never serialized (it is code held by name in the
	// engine's registry); a declarative node serializes its kind + config so the
	// body is rebuildable from the log via the resolver.
	BodyKind   string          `json:"body_kind,omitempty"`
	BodyConfig json.RawMessage `json:"body_config,omitempty"`
}

type scopeSpec struct {
	Name     string         `json:"name"`
	Root     string         `json:"root,omitempty"`
	Budget   map[string]int `json:"budget,omitempty"`
	Detached bool           `json:"detached,omitempty"`
}

type projectionSpec struct {
	Name   string          `json:"name"`
	On     []string        `json:"on,omitempty"`
	In     Horizon         `json:"in,omitempty"`
	Type   string          `json:"type,omitempty"`
	Key    string          `json:"key,omitempty"`
	Value  string          `json:"value,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

// selfMutationReasons enforces the FOREIGN-SCOPE rule (doc 31 §4): a changeset
// issued from inside scope X may compose OTHER scopes' topology but may not
// add/modify a subscriber/scope/projection that lives in X itself — a running
// cone is never rewritten out from under itself, and the authority boundary is
// "you compose downstream, you do not self-modify." issuing is the set of scope
// names the requesting changeset.requested event is a member of (its cones). It
// returns one human-readable reason per offending decl; an empty result means the
// changeset only touches foreign scopes (allowed). Event decls are catalog-global
// (no scope) and a subscriber/projection in `global` targets no single cone, so
// both are exempt. This is checked only for in-graph changesets — an operator
// Apply has no issuing cone, so issuing is empty and nothing is flagged.
func selfMutationReasons(issuing map[string]struct{}, decls []Decl) []string {
	if len(issuing) == 0 {
		return nil
	}
	flagged := func(scope, kind, name string) string {
		return fmt.Sprintf(
			"changeset may not modify its OWN scope %q (doc 31 §4 foreign-scope rule): %s %q targets the issuing cone — build a DIFFERENT scope, or have an operator apply this",
			scope, kind, name)
	}
	var out []string
	for _, d := range decls {
		switch v := d.(type) {
		case Subscriber:
			in := v.In
			if in == "" || in == "global" {
				continue
			}
			if _, ok := issuing[in]; ok {
				out = append(out, flagged(in, "subscriber", v.Name))
			}
		case Scope:
			if _, ok := issuing[v.Name]; ok {
				out = append(out, flagged(v.Name, "scope", v.Name))
			}
		case Projection:
			in := string(v.In)
			if in == "" || in == "global" {
				continue
			}
			if _, ok := issuing[in]; ok {
				out = append(out, flagged(in, "projection", v.Name))
			}
		}
	}
	return out
}

// opsOf serializes a batch of Decls into add-ops (doc 20). An EventKind becomes
// an "event" op; the three wiring kinds become their respective ops. A Decl the
// changeset model does not manage is skipped (there is no fourth wiring kind).
func opsOf(decls []Decl) []Op {
	out := make([]Op, 0, len(decls))
	for _, d := range decls {
		switch v := d.(type) {
		case Subscriber:
			out = append(out, Op{Verb: verbAdd, Kind: opKindSubscriber, Name: v.Name, Spec: mustMarshal(subscriberSpecOf(v))})
		case Scope:
			out = append(out, Op{Verb: verbAdd, Kind: opKindScope, Name: v.Name, Spec: mustMarshal(scopeSpec{Name: v.Name, Root: v.Root, Budget: v.Budget, Detached: v.Detached})})
		case Projection:
			out = append(out, Op{Verb: verbAdd, Kind: opKindProjection, Name: v.Name, Spec: mustMarshal(projectionSpecOf(v))})
		case EventKind:
			out = append(out, Op{Verb: verbAdd, Kind: opKindEvent, Name: v.Kind, Spec: mustMarshal(registration{Kind: v.Kind, Schema: v.Schema, Terminal: v.Terminal})})
		}
	}
	return out
}

// OpsOf is the exported changeset serializer (opsOf): it turns a batch of Decls
// into add-ops, the unit a node emits to DRIVE a topology changeset in-graph.
// The reflex-side bridge that translates a topology document into a changeset
// request builds the request payload from OpsOf(decls); the engine parses it back
// with declsOfOps and runs the same validate+commit pipeline as an operator Apply.
func OpsOf(decls []Decl) []Op { return opsOf(decls) }

// ChangesetRequestPayload builds the payload of an in-graph changeset request
// (the KindChangesetRequested event a node emits): the add-ops for decls plus a
// principal tag. The engine's in-graph dispatch hook unmarshals exactly this.
func ChangesetRequestPayload(decls []Decl, principal string) json.RawMessage {
	return mustMarshal(changesetPayload{Ops: opsOf(decls), Principal: principal})
}

// opsFromPayload reads the ops out of a changeset request payload (the inverse of
// ChangesetRequestPayload's marshal). A malformed payload yields no ops, so the
// resulting empty changeset is a connected no-op rather than a crash.
func opsFromPayload(payload json.RawMessage) []Op {
	var p changesetPayload
	_ = json.Unmarshal(payload, &p)
	return p.Ops
}

// declsOfOps rebuilds Decls from add-ops — the inverse of opsOf — for the
// in-graph changeset path: a node emits ops, the engine turns them back into
// decls to run the same validate+commit pipeline an operator Apply runs. Bodies
// ride as their descriptor (BodyKind+BodyConfig), resolved by the engine's
// resolver exactly as a loaded-from-log node is. Remove-ops are not modelled
// in-graph yet (a node grows a topology); a remove yields an error so the
// changeset is cleanly rejected rather than silently dropped.
func declsOfOps(ops []Op) ([]Decl, error) {
	var out []Decl
	for _, op := range ops {
		if op.Verb == verbRemove {
			return nil, fmt.Errorf("engine: in-graph changeset: remove op (%s %q) not supported", op.Kind, op.Name)
		}
		switch op.Kind {
		case opKindSubscriber:
			var s subscriberSpec
			if err := json.Unmarshal(op.Spec, &s); err != nil {
				return nil, fmt.Errorf("engine: changeset op subscriber %q: %w", op.Name, err)
			}
			out = append(out, Subscriber{
				Name: s.Name, On: s.On, In: s.In, Reads: s.Reads, Emits: s.Emits, Scope: s.Scope,
				BodyKind: s.BodyKind, BodyConfig: s.BodyConfig,
			})
		case opKindScope:
			var s scopeSpec
			if err := json.Unmarshal(op.Spec, &s); err != nil {
				return nil, fmt.Errorf("engine: changeset op scope %q: %w", op.Name, err)
			}
			out = append(out, Scope{Name: s.Name, Root: s.Root, Budget: s.Budget, Detached: s.Detached})
		case opKindProjection:
			var s projectionSpec
			if err := json.Unmarshal(op.Spec, &s); err != nil {
				return nil, fmt.Errorf("engine: changeset op projection %q: %w", op.Name, err)
			}
			out = append(out, Projection{Name: s.Name, On: s.On, In: s.In, Type: s.Type, Key: s.Key, Value: s.Value, Params: s.Params})
		case opKindEvent:
			var r registration
			if err := json.Unmarshal(op.Spec, &r); err != nil {
				return nil, fmt.Errorf("engine: changeset op event %q: %w", op.Name, err)
			}
			out = append(out, EventKind{Kind: r.Kind, Schema: r.Schema, Terminal: r.Terminal})
		}
	}
	return out, nil
}

func subscriberSpecOf(n Subscriber) subscriberSpec {
	return subscriberSpec{
		Name: n.Name, On: n.On, In: n.In, Reads: n.Reads, Emits: n.Emits, Scope: n.Scope,
		BodyKind: n.BodyKind, BodyConfig: n.BodyConfig,
	}
}

func projectionSpecOf(p Projection) projectionSpec {
	return projectionSpec{Name: p.Name, On: p.On, In: p.In, Type: p.Type, Key: p.Key, Value: p.Value, Params: p.Params}
}

// factOf maps an op to the engine fact that records it: the subject the engine
// appends on a successful apply, and its payload. An add carries the spec as the
// payload (so foldTopology reads the spec straight back); a remove carries just
// the name. The event op reuses the catalog registration payload {kind, schema}
// so the single sys.event.registered fact serves both the catalog fold and the
// topology fold.
func factOf(op Op) (subject string, payload json.RawMessage) {
	switch op.Kind {
	case opKindSubscriber:
		if op.Verb == verbRemove {
			return subjSubscriberDeregistered, mustMarshal(map[string]string{"name": op.Name})
		}
		return subjSubscriberRegistered, op.Spec
	case opKindScope:
		if op.Verb == verbRemove {
			return subjScopeRetired, mustMarshal(map[string]string{"name": op.Name})
		}
		return subjScopeDeclared, op.Spec
	case opKindProjection:
		if op.Verb == verbRemove {
			return subjProjectionDeregistered, mustMarshal(map[string]string{"name": op.Name})
		}
		return subjProjectionRegistered, op.Spec
	case opKindEvent:
		// Removal of a catalog entry is not modelled (the catalog is
		// last-writer-wins, additive); an add registers/evolves the kind.
		return subjEventRegistered, op.Spec
	default:
		return "", nil
	}
}

// foldTopology reconstructs the live topology as a []Decl by folding the
// engine-written facts on the log (doc 20: "the live table folds only facts").
// It is the WHOLE definition of the live table — the cached e.live is a
// memoisation of exactly this (G8). Registration facts add/replace by name (a
// later registration wins — schema/wiring evolution); deregistration/retirement
// facts remove by name. Insertion order is preserved per object kind so the fold
// is deterministic. A Node's Body is reattached from the bodies registry by
// name — the one part that is code, not a fact.
func foldTopology(log []Event, bodies map[string]Reaction) []Decl {
	type orderedNodes struct {
		m     map[string]subscriberSpec
		order []string
	}
	nodes := orderedNodes{m: map[string]subscriberSpec{}}
	scopes := map[string]scopeSpec{}
	var scopeOrder []string
	projs := map[string]projectionSpec{}
	var projOrder []string
	events := map[string]eventReg{}
	var eventOrder []string

	addOrder := func(order *[]string, present map[string]bool, name string) {
		if !present[name] {
			present[name] = true
			*order = append(*order, name)
		}
	}
	nodePresent := map[string]bool{}
	scopePresent := map[string]bool{}
	projPresent := map[string]bool{}
	eventPresent := map[string]bool{}

	for _, ev := range log {
		switch ev.Subject {
		case subjSubscriberRegistered:
			var s subscriberSpec
			if json.Unmarshal(ev.Payload, &s) == nil && s.Name != "" {
				addOrder(&nodes.order, nodePresent, s.Name)
				nodes.m[s.Name] = s
			}
		case subjSubscriberDeregistered:
			delete(nodes.m, nameField(ev.Payload))
		case subjScopeDeclared:
			var s scopeSpec
			if json.Unmarshal(ev.Payload, &s) == nil && s.Name != "" {
				addOrder(&scopeOrder, scopePresent, s.Name)
				scopes[s.Name] = s
			}
		case subjScopeRetired:
			delete(scopes, nameField(ev.Payload))
		case subjProjectionRegistered:
			var s projectionSpec
			if json.Unmarshal(ev.Payload, &s) == nil && s.Name != "" {
				addOrder(&projOrder, projPresent, s.Name)
				projs[s.Name] = s
			}
		case subjProjectionDeregistered:
			delete(projs, nameField(ev.Payload))
		case subjEventRegistered:
			if kind, schema, terminal, ok := parseRegistration(ev.Payload); ok && kind != "" {
				addOrder(&eventOrder, eventPresent, kind)
				events[kind] = eventReg{schema: schema, terminal: terminal}
			}
		}
	}

	var out []Decl
	for _, name := range scopeOrder {
		if s, ok := scopes[name]; ok {
			out = append(out, Scope{Name: s.Name, Root: s.Root, Budget: s.Budget, Detached: s.Detached})
		}
	}
	for _, name := range nodes.order {
		s, ok := nodes.m[name]
		if !ok {
			continue
		}
		out = append(out, Subscriber{
			Name: s.Name, On: s.On, In: s.In, Reads: s.Reads, Emits: s.Emits, Scope: s.Scope,
			BodyKind: s.BodyKind, BodyConfig: s.BodyConfig,
			Body: bodies[s.Name],
		})
	}
	for _, name := range projOrder {
		if s, ok := projs[name]; ok {
			out = append(out, Projection{Name: s.Name, On: s.On, In: s.In, Type: s.Type, Key: s.Key, Value: s.Value, Params: s.Params})
		}
	}
	for _, kind := range eventOrder {
		if reg, ok := events[kind]; ok {
			out = append(out, EventKind{Kind: kind, Schema: reg.schema, Terminal: reg.terminal})
		}
	}
	return out
}

// eventReg is the folded form of an event-catalog registration on the log: the
// kind's schema and its terminal-leaf flag, both restored from the
// sys.event.registered fact (registration carries terminal so the fold preserves
// it).
type eventReg struct {
	schema   json.RawMessage
	terminal bool
}

// nameField reads the {"name": ...} payload of a deregistration/retirement fact.
func nameField(payload json.RawMessage) string {
	var m struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(payload, &m)
	return m.Name
}
