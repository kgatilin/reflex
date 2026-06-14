package engine

import "encoding/json"

// The control plane (doc 20 / CONCEPT §8): managing the topology — adding or
// removing nodes, subscriptions, scopes, projections, and event types — is
// itself events on the log, not a config edit + restart. A client never mutates
// the live table; it REQUESTS a changeset, the engine validates the resulting
// graph as a whole, and on success the engine writes the facts the live table
// folds:
//
//	sys.topology.changeset.requested{ ops, principal }
//	   → engine validates fold(live table) + ops → resulting graph
//	   → facts:  sys.node.registered · sys.scope.declared · sys.projection.registered
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

	subjNodeRegistered         = "sys.node.registered"
	subjNodeDeregistered       = "sys.node.deregistered"
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
	Kind string          `json:"kind"` // "node" | "scope" | "projection" | "event"
	Name string          `json:"name"`
	Spec json.RawMessage `json:"spec,omitempty"`
}

const (
	verbAdd    = "add"
	verbRemove = "remove"

	opKindNode       = "node"
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

// nodeSpec is the serializable wiring of a Node — everything but Body (doc 20 /
// changeset.go package doc). The Body is reattached from the engine's registry
// by Name at fold time.
type nodeSpec struct {
	Name  string   `json:"name"`
	On    []string `json:"on,omitempty"`
	In    string   `json:"in,omitempty"`
	Reads []string `json:"reads,omitempty"`
	Emits []string `json:"emits,omitempty"`
	Scope string   `json:"scope,omitempty"`
}

type scopeSpec struct {
	Name   string         `json:"name"`
	Root   string         `json:"root,omitempty"`
	Budget map[string]int `json:"budget,omitempty"`
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

// opsOf serializes a batch of Decls into add-ops (doc 20). An EventKind becomes
// an "event" op; the three wiring kinds become their respective ops. A Decl the
// changeset model does not manage is skipped (there is no fourth wiring kind).
func opsOf(decls []Decl) []Op {
	out := make([]Op, 0, len(decls))
	for _, d := range decls {
		switch v := d.(type) {
		case Node:
			out = append(out, Op{Verb: verbAdd, Kind: opKindNode, Name: v.Name, Spec: mustMarshal(nodeSpecOf(v))})
		case Scope:
			out = append(out, Op{Verb: verbAdd, Kind: opKindScope, Name: v.Name, Spec: mustMarshal(scopeSpec{Name: v.Name, Root: v.Root, Budget: v.Budget})})
		case Projection:
			out = append(out, Op{Verb: verbAdd, Kind: opKindProjection, Name: v.Name, Spec: mustMarshal(projectionSpecOf(v))})
		case EventKind:
			out = append(out, Op{Verb: verbAdd, Kind: opKindEvent, Name: v.Kind, Spec: mustMarshal(registration{Kind: v.Kind, Schema: v.Schema})})
		}
	}
	return out
}

func nodeSpecOf(n Node) nodeSpec {
	return nodeSpec{Name: n.Name, On: n.On, In: n.In, Reads: n.Reads, Emits: n.Emits, Scope: n.Scope}
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
	case opKindNode:
		if op.Verb == verbRemove {
			return subjNodeDeregistered, mustMarshal(map[string]string{"name": op.Name})
		}
		return subjNodeRegistered, op.Spec
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
		m     map[string]nodeSpec
		order []string
	}
	nodes := orderedNodes{m: map[string]nodeSpec{}}
	scopes := map[string]scopeSpec{}
	var scopeOrder []string
	projs := map[string]projectionSpec{}
	var projOrder []string
	events := map[string]json.RawMessage{}
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
		case subjNodeRegistered:
			var s nodeSpec
			if json.Unmarshal(ev.Payload, &s) == nil && s.Name != "" {
				addOrder(&nodes.order, nodePresent, s.Name)
				nodes.m[s.Name] = s
			}
		case subjNodeDeregistered:
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
			if kind, schema, ok := parseRegistration(ev.Payload); ok && kind != "" {
				addOrder(&eventOrder, eventPresent, kind)
				events[kind] = schema
			}
		}
	}

	var out []Decl
	for _, name := range scopeOrder {
		if s, ok := scopes[name]; ok {
			out = append(out, Scope{Name: s.Name, Root: s.Root, Budget: s.Budget})
		}
	}
	for _, name := range nodes.order {
		s, ok := nodes.m[name]
		if !ok {
			continue
		}
		out = append(out, Node{
			Name: s.Name, On: s.On, In: s.In, Reads: s.Reads, Emits: s.Emits, Scope: s.Scope,
			Body: bodies[s.Name],
		})
	}
	for _, name := range projOrder {
		if s, ok := projs[name]; ok {
			out = append(out, Projection{Name: s.Name, On: s.On, In: s.In, Type: s.Type, Key: s.Key, Value: s.Value, Params: s.Params})
		}
	}
	for _, kind := range eventOrder {
		if schema, ok := events[kind]; ok {
			out = append(out, EventKind{Kind: kind, Schema: schema})
		}
	}
	return out
}

// nameField reads the {"name": ...} payload of a deregistration/retirement fact.
func nameField(payload json.RawMessage) string {
	var m struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(payload, &m)
	return m.Name
}
