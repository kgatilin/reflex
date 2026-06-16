// Package changeset is the bridge body (body kind "changeset"): the in-graph
// control plane an agent uses to BUILD topology at runtime, as a sequence of
// small STRUCTURED, COMPOSABLE pieces rather than one big document.
//
// A reasoning node (an llm brain) calls typed functions — one per declaration
// kind — to add pieces to a DRAFT topology, then commits the draft:
//
//	topology.scope.add{name, root, detached, budget}
//	topology.event.add{kind, terminal, schema}
//	topology.subscriber.add{name, on, in, emits, reads, body}
//	topology.projection.add{name, on, in, type, params}
//	topology.build{}                          — validate + commit the whole draft
//
// Each add-* carries a PROPER JSON SCHEMA (Catalog below), so a function-calling
// model is forced to produce well-formed structured data — not free-text YAML. The
// pieces accumulate on the LOG; a `log` projection (the bridge's `reads:` draft)
// folds them, so the draft is a projection over the add events, no hidden state.
//
// The bridge ACKs every add with `topology.piece.added` (so the brain re-drives and
// adds the next piece — the add-* behave like a tool loop), and on `topology.build`
// it reads the draft, assembles the engine.Decls, and emits
// engine.KindChangesetRequested. The engine's in-graph hook then validates the
// WHOLE resulting graph and commits it (engine.commitChangeset) — a subgraph the
// draft adds goes live in the same drain (engine live-refresh). On a parse/decl
// error it emits the FAIL kind carrying {error}; the engine emits
// changeset.applied / .rejected the brain hears for the build outcome.
//
// Config (BodyConfig), all optional:
//
//	emit:      the changeset-request kind to emit (default engine.KindChangesetRequested).
//	fail:      the kind emitted on a build error, carrying {error} (default
//	           "topology.apply.failed").
//	ack:       the kind emitted to ACK each add (default "topology.piece.added").
//	principal: the principal stamped on the changeset request (default "in-graph").
//	draft:     the `log` projection name the bridge reads to assemble the draft on
//	           build (default "topology.draft").
package changeset

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kgatilin/reflex/engine"
)

// The structured control kinds — the typed functions the brain calls to compose a
// draft topology, plus the commit. Exported so a topology/test can name them.
const (
	KindScopeAdd      = "topology.scope.add"
	KindEventAdd      = "topology.event.add"
	KindSubscriberAdd = "topology.subscriber.add"
	KindProjectionAdd = "topology.projection.add"
	KindBuild         = "topology.build"
)

// Config is the changeset bridge descriptor (all fields optional; see package doc).
type Config struct {
	Emit      string `json:"emit,omitempty"`
	Fail      string `json:"fail,omitempty"`
	Principal string `json:"principal,omitempty"`
	Draft     string `json:"draft,omitempty"`
}

const (
	defaultFail  = "topology.apply.failed"
	defaultDraft = "topology.draft"
	defaultPrinc = "in-graph"
)

// structuredSchemas are the per-piece parameter schemas the bridge advertises
// (point of the whole design: typed function-calling, not a text blob). Keyed by
// control kind; the build kind takes no params.
var structuredSchemas = map[string]json.RawMessage{
	KindScopeAdd: json.RawMessage(`{"type":"object","properties":{` +
		`"name":{"type":"string","description":"scope name"},` +
		`"root":{"type":"string","description":"the event kind whose dispatch roots an instance of this scope"},` +
		`"detached":{"type":"boolean","description":"true = a top-level SIBLING cone; use this for the worker scope you dispatch into so it is isolated from you"},` +
		`"budget":{"type":"object","description":"per-kind ceiling within the cone that bounds loops, e.g. {\"llm.message\":8,\"tool.fs.read.call\":80}","additionalProperties":{"type":"integer"}}},` +
		`"required":["name","root"]}`),
	KindEventAdd: json.RawMessage(`{"type":"object","properties":{` +
		`"kind":{"type":"string","description":"the event kind to register"},` +
		`"terminal":{"type":"boolean","description":"true = a graph output / leaf that needs no consumer (e.g. request.terminal, llm.usage)"},` +
		`"schema":{"type":"object","description":"JSON Schema of this event's payload; it is advertised as the function parameters to any llm that emits this kind"}},` +
		`"required":["kind"]}`),
	KindSubscriberAdd: json.RawMessage(`{"type":"object","properties":{` +
		`"name":{"type":"string","description":"subscriber name; name it EXACTLY after a *.call tool kind to get a host-backed tool (then omit body)"},` +
		`"on":{"type":"array","items":{"type":"string"},"description":"event kinds this subscriber reacts to"},` +
		`"in":{"type":"string","description":"scope name it runs in, or \"global\""},` +
		`"emits":{"type":"array","items":{"type":"string"},"description":"the event kinds it may emit (an llm's function menu)"},` +
		`"reads":{"type":"array","items":{"type":"string"},"description":"projection names it reads (e.g. its history)"},` +
		`"body":{"type":"object","description":"the behaviour: {kind:llm|entry|gate, config:{...}}. OMIT for a host-backed tool subscriber.","properties":{"kind":{"type":"string"},"config":{"type":"object"}}}},` +
		`"required":["name"]}`),
	KindProjectionAdd: json.RawMessage(`{"type":"object","properties":{` +
		`"name":{"type":"string"},` +
		`"on":{"type":"array","items":{"type":"string"},"description":"event kinds folded into the view"},` +
		`"in":{"type":"string","description":"horizon: a scope name or \"global\""},` +
		`"type":{"type":"string","description":"view type, e.g. llm.history or log"},` +
		`"params":{"type":"object","description":"builder params, e.g. for llm.history {system, answer, task_kinds, emits}"}},` +
		`"required":["name"]}`),
	KindBuild: json.RawMessage(`{"type":"object","properties":{},` +
		`"description":"validate + commit the accumulated draft as ONE topology; the subgraph goes live. Call this after you have added every scope, event, subscriber and projection."}`),
}

// Catalog self-describes the bridge's tools (a nodes.Describer): for every control
// kind in the subscriber's On it registers the kind WITH its structured schema so a
// reasoning node advertises a typed function; plus the ACK and FAIL kinds it emits
// (registered so subscriptions to them are not dead). The changeset-request kind is
// engine-owned. Register with nodes.RegisterCatalog("changeset", changeset.Catalog).
func Catalog(s engine.Subscriber) []engine.EventKind {
	var cfg Config
	if len(s.BodyConfig) > 0 {
		_ = json.Unmarshal(s.BodyConfig, &cfg)
	}
	out := []engine.EventKind{
		{Kind: orDefault(cfg.Fail, defaultFail)}, // bridge → brain: build-failure feedback
	}
	for _, k := range s.On { // the structured control functions the brain calls
		if schema, ok := structuredSchemas[k]; ok {
			out = append(out, engine.EventKind{Kind: k, Schema: schema})
		}
	}
	return out
}

// Factory is the nodes.Factory for body kind "changeset". On an add-* kind it ACKs
// (the piece is already on the log; the draft projection folds it). On the build
// kind it reads the draft, assembles the decls, and emits the changeset request.
func Factory(s engine.Subscriber) (engine.Reaction, error) {
	var cfg Config
	if len(s.BodyConfig) > 0 {
		if err := json.Unmarshal(s.BodyConfig, &cfg); err != nil {
			return nil, fmt.Errorf("changeset: body config: %w", err)
		}
	}
	emit := orDefault(cfg.Emit, engine.KindChangesetRequested)
	fail := orDefault(cfg.Fail, defaultFail)
	principal := orDefault(cfg.Principal, defaultPrinc)
	draft := orDefault(cfg.Draft, defaultDraft)

	return engine.ReactionFunc(func(_ context.Context, ev engine.Event, views engine.Views) ([]engine.Emit, error) {
		if engine.KindOf(ev) != KindBuild {
			// An add-* piece: it is already on the log (the draft projection folds
			// the whole cone), so it just accumulates — NO emit. Acking each add
			// would re-drive the brain once PER add, and since a model emits several
			// add calls in one turn (siblings), that branches the brain into many
			// parallel chains. Instead the brain re-drives only on the BUILD outcome
			// (changeset.applied/.rejected) — one linear loop: add… add… build.
			return nil, nil
		}
		// Build: fold the draft and commit the whole topology as one changeset.
		events := engine.ViewAs[[]engine.Event](views, draft)
		decls, err := declsFromDraft(events)
		if err != nil {
			return []engine.Emit{failEmit(fail, err)}, nil
		}
		if len(decls) == 0 {
			return []engine.Emit{failEmit(fail, fmt.Errorf(
				"the draft is empty — add pieces first with topology.subscriber.add / topology.scope.add / topology.event.add / topology.projection.add, THEN call topology.build"))}, nil
		}
		return []engine.Emit{{Kind: emit, Payload: engine.ChangesetRequestPayload(decls, principal)}}, nil
	}), nil
}

// declsFromDraft turns the accumulated add-* events (the draft, a `log` view) into
// engine.Decls, decoding each by its kind. A malformed piece aborts the build with
// an error naming the offending kind — the brain re-adds a corrected piece.
func declsFromDraft(events []engine.Event) ([]engine.Decl, error) {
	var out []engine.Decl
	for _, ev := range events {
		kind := engine.KindOf(ev)
		switch kind {
		case KindScopeAdd:
			var p scopeArgs
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				return nil, fmt.Errorf("%s: %w", kind, err)
			}
			out = append(out, engine.Scope{Name: p.Name, Root: p.Root, Detached: p.Detached, Budget: p.Budget})
		case KindEventAdd:
			var p eventArgs
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				return nil, fmt.Errorf("%s: %w", kind, err)
			}
			out = append(out, engine.EventKind{Kind: p.Kind, Terminal: p.Terminal, Schema: p.Schema})
		case KindSubscriberAdd:
			var p subscriberArgs
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				return nil, fmt.Errorf("%s: %w", kind, err)
			}
			n := engine.Subscriber{Name: p.Name, On: p.On, In: p.In, Emits: p.Emits, Reads: p.Reads}
			if p.Body != nil {
				n.BodyKind = p.Body.Kind
				n.BodyConfig = p.Body.Config
			}
			out = append(out, n)
		case KindProjectionAdd:
			var p projectionArgs
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				return nil, fmt.Errorf("%s: %w", kind, err)
			}
			out = append(out, engine.Projection{Name: p.Name, On: p.On, In: engine.Horizon(p.In), Type: p.Type, Params: p.Params})
		}
	}
	return out, nil
}

// The structured function-arg shapes — one per control kind. These ARE the schemas
// in structuredSchemas, decoded.
type scopeArgs struct {
	Name     string         `json:"name"`
	Root     string         `json:"root"`
	Detached bool           `json:"detached"`
	Budget   map[string]int `json:"budget"`
}

type eventArgs struct {
	Kind     string          `json:"kind"`
	Terminal bool            `json:"terminal"`
	Schema   json.RawMessage `json:"schema"`
}

type bodyArgs struct {
	Kind   string          `json:"kind"`
	Config json.RawMessage `json:"config"`
}

type subscriberArgs struct {
	Name  string    `json:"name"`
	On    []string  `json:"on"`
	In    string    `json:"in"`
	Emits []string  `json:"emits"`
	Reads []string  `json:"reads"`
	Body  *bodyArgs `json:"body"`
}

type projectionArgs struct {
	Name   string          `json:"name"`
	On     []string        `json:"on"`
	In     string          `json:"in"`
	Type   string          `json:"type"`
	Params json.RawMessage `json:"params"`
}

// failEmit builds the build-failure feedback event carrying the error text.
func failEmit(kind string, err error) engine.Emit {
	p, _ := json.Marshal(map[string]string{"error": err.Error()})
	return engine.Emit{Kind: kind, Payload: p}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
