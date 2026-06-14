// Package llm is the reasoning body (docs/24-concept.md §3, doc 26 §4): a node
// whose body calls a model once and turns the completion into allowlisted
// emits. It is NOT special — it is an ordinary engine.Node with an Emits
// allowlist (the "menu" — there is no separate tool-menu concept, doc 26 §4)
// whose Body happens to call pkg/provider. Tool-calling is transport encoding:
// a function-call decodes into Emit{Kind: "tool.X.call"}, a text completion
// into Emit{Kind: answerKind} — both allowlisted emissions.
//
// The model's prompt (system + messages) is shaped by a view, not the body:
// this package registers the "llm.history" view type (doc 26 §4b), and the body
// just reads it through engine.ViewAs and hands System()/Messages() to the
// provider. Swapping the prompt-assembly strategy is swapping the view, not the
// body.
package llm

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/kgatilin/reflex/engine"
	"github.com/kgatilin/reflex/pkg/provider"
)

// DefaultAnswerKind is the fixed kind a text completion decodes into (doc 23 /
// 26 §4): the model's prose answer is one llm.message fact. Everything else the
// model produces is a function-call → an allowlisted Emit.
const DefaultAnswerKind = "llm.message"

// History is the view type the llm body reads (doc 26 §4b): the cone shaped into
// a frozen system preamble and an append-only message tail. The body is dumb —
// it calls System()/Messages() and hands them to the provider; the split lives
// in the registered builder, swappable independent of the body.
type History interface {
	System() string
	Messages() []provider.Message
}

func init() {
	engine.RegisterType("llm.history", buildHistory)
}

// historyParams is the builder configuration carried in Projection.Params (doc
// 26 §4b): the static system base, the emit allowlist (for the assistant-turn
// boundary and role assignment), the answer kind, and the kinds that count as
// the user task. The llm constructor fills it from the node's Config so the view
// and the node stay consistent.
type historyParams struct {
	System    string   `json:"system"`
	Emits     []string `json:"emits"`
	Answer    string   `json:"answer"`
	TaskKinds []string `json:"task_kinds"`
}

// buildHistory is the "llm.history" view-type builder (doc 26 §4b): the
// positional system/message split over the matched events (the cone, in log
// order). It is a pure function of the events + params → recomputable (G8).
//
//   - Boundary = the first assistant turn = the first event whose kind matches
//     the node's Emits (the model's first action — text or a tool.X.call).
//   - Before the boundary: the user task (kind ∈ TaskKinds) → the first user
//     message; everything else → the frozen System() preamble.
//   - From the boundary onward: the append-only tail, role by Emits-membership
//     (a kind the node emits → assistant, else → user), in log order.
func buildHistory(p engine.Projection, events []engine.Event) any {
	var params historyParams
	if len(p.Params) > 0 {
		_ = json.Unmarshal(p.Params, &params)
	}

	isAssistant := func(kind string) bool { return matchesAny(params.Emits, kind) }
	isTask := func(kind string) bool { return matchesAny(params.TaskKinds, kind) }

	// Boundary: first assistant-side event (first own-emit). None ⇒ all preamble.
	boundary := len(events)
	for i, ev := range events {
		if isAssistant(engine.KindOf(ev)) {
			boundary = i
			break
		}
	}

	h := &history{system: params.System}
	var preambleBlocks []string
	// Pre-boundary: task → first user message; the rest → frozen system preamble.
	for _, ev := range events[:boundary] {
		kind := engine.KindOf(ev)
		if isTask(kind) {
			h.messages = append(h.messages, provider.Message{Role: "user", Text: messageText(ev)})
			continue
		}
		preambleBlocks = append(preambleBlocks, "["+kind+"]\n"+messageText(ev))
	}
	if len(preambleBlocks) > 0 {
		if h.system != "" {
			h.system += "\n\n"
		}
		h.system += strings.Join(preambleBlocks, "\n\n")
	}
	// Post-boundary: append-only tail, role by Emits-membership.
	for _, ev := range events[boundary:] {
		role := "user"
		if isAssistant(engine.KindOf(ev)) {
			role = "assistant"
		}
		h.messages = append(h.messages, provider.Message{Role: role, Text: messageText(ev)})
	}
	return h
}

// matchesAny reports whether any pattern matches the kind (NATS grammar via the
// engine matcher) — the same membership test a node's On uses.
func matchesAny(patterns []string, kind string) bool {
	for _, pat := range patterns {
		if engine.MatchKind(pat, kind) {
			return true
		}
	}
	return false
}

// messageText renders an event payload to provider message text: the "text"
// field if present (the prose convention), else the compact payload JSON. Total
// and deterministic — never panics on an odd payload.
func messageText(ev engine.Event) string {
	if len(ev.Payload) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(ev.Payload, &m); err == nil {
		if raw, ok := m["text"]; ok {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				return s
			}
		}
	}
	return string(ev.Payload)
}

// history is the materialised History (doc 26 §4b): a frozen system preamble and
// the message tail, both computed once at read time from the cone.
type history struct {
	system   string
	messages []provider.Message
}

func (h *history) System() string               { return h.system }
func (h *history) Messages() []provider.Message { return h.messages }

// Config wires one llm seat: its subscription/emit topology plus its model
// binding and prompt shaping (doc 26 §4). It is behaviour, not a node-kind —
// the result is an ordinary engine.Node.
type Config struct {
	Name  string   // node name
	On    []string // subscription patterns
	In    string   // scope qualifier (e.g. "request"); empty ⇒ global
	Scope string   // non-empty ⇒ this firing roots the named scope
	Emits []string // emit allowlist — the menu (doc 26 §4)

	// Model binding + backend config (pkg/provider, reused from stage 0).
	Model     string // "vertex:anthropic/claude-opus-4-8"
	Project   string
	Location  string
	System    string // static system base (the seat's identity)
	MaxTokens int

	// Answer is the kind a text completion decodes into (default llm.message).
	Answer string
	// TaskKinds count as the user task in the history preamble (default
	// {"request.received"}).
	TaskKinds []string
	// HistoryName names the llm.history projection this seat reads (default
	// Name+".history"); HistoryOn are its transcript patterns; HistoryIn its
	// horizon (default request).
	HistoryName string
	HistoryOn   []string
	HistoryIn   engine.Horizon
}

// defaults fills the optional Config fields with their conventional values.
func (c Config) defaults() Config {
	if c.Answer == "" {
		c.Answer = DefaultAnswerKind
	}
	if len(c.TaskKinds) == 0 {
		c.TaskKinds = []string{"request.received"}
	}
	if c.HistoryName == "" {
		c.HistoryName = c.Name + ".history"
	}
	if c.HistoryIn == "" {
		c.HistoryIn = engine.HorizonRequest
	}
	if len(c.HistoryOn) == 0 {
		// A broad transcript: the task, the agent's own voice, tool I/O, and the
		// state/context facts that make up the preamble. Over-selection is safe —
		// the builder's split decides system vs message.
		c.HistoryOn = []string{
			"request.received", "user.message", "llm.message",
			"tool.>", "state.updated.>", "context.found",
		}
	}
	return c
}

// NewWithProvider builds the seat's node and its default llm.history projection
// against an injected provider (the test/stub seam; production uses New). The
// node reads the projection; the projection carries the split params derived
// from the same Config, so view and node never drift.
func NewWithProvider(cfg Config, p provider.Provider) (engine.Node, engine.Projection) {
	cfg = cfg.defaults()
	node := engine.Node{
		Name:  cfg.Name,
		On:    cfg.On,
		In:    cfg.In,
		Scope: cfg.Scope,
		Emits: cfg.Emits,
		Reads: []string{cfg.HistoryName},
		Body:  body(cfg, p),
	}
	proj := engine.Projection{
		Name: cfg.HistoryName,
		On:   cfg.HistoryOn,
		In:   cfg.HistoryIn,
		Type: "llm.history",
		Params: mustMarshal(historyParams{
			System:    cfg.System,
			Emits:     cfg.Emits,
			Answer:    cfg.Answer,
			TaskKinds: cfg.TaskKinds,
		}),
	}
	return node, proj
}

// Declare builds the DESCRIPTOR form of an llm seat (doc 20 / CONCEPT §8): a
// node carrying body kind "llm" + this Config as its serialized descriptor
// (BodyConfig), plus its paired llm.history projection — no provider is resolved
// here. This is what the declarative/daemon path applies: the body is rebuilt
// from the descriptor by llm.Factory through the engine's resolver, so the seat
// is recoverable from the log (G8). The in-process analog is NewWithProvider
// (a live Body, no descriptor). The Config round-trips through the descriptor by
// field name (it is both marshalled here and unmarshalled in Factory).
func Declare(cfg Config) (engine.Node, engine.Projection) {
	cfg = cfg.defaults()
	node := engine.Node{
		Name:       cfg.Name,
		On:         cfg.On,
		In:         cfg.In,
		Scope:      cfg.Scope,
		Emits:      cfg.Emits,
		Reads:      []string{cfg.HistoryName},
		BodyKind:   "llm",
		BodyConfig: mustMarshal(cfg),
	}
	proj := engine.Projection{
		Name: cfg.HistoryName,
		On:   cfg.HistoryOn,
		In:   cfg.HistoryIn,
		Type: "llm.history",
		Params: mustMarshal(historyParams{
			System:    cfg.System,
			Emits:     cfg.Emits,
			Answer:    cfg.Answer,
			TaskKinds: cfg.TaskKinds,
		}),
	}
	return node, proj
}

// New resolves the provider from the model binding (pkg/provider) and builds the
// seat. The returned model id replaces the binding in completion requests.
func New(cfg Config) (engine.Node, engine.Projection, error) {
	p, model, err := provider.For(cfg.Model, provider.Config{Project: cfg.Project, Location: cfg.Location})
	if err != nil {
		return engine.Node{}, engine.Projection{}, err
	}
	cfg.Model = model
	n, pr := NewWithProvider(cfg, p)
	return n, pr, nil
}

// Reaction resolves the provider and returns JUST the seat's body Reaction —
// the factory path (doc 20 / CONCEPT §8): a declarative node carries body kind
// "llm" + this Config as its descriptor, and the resolver builds the Reaction
// from it. The paired llm.history projection (NewWithProvider's second return)
// is declared SEPARATELY in the topology — the body only reads it by name
// (cfg.HistoryName), so the descriptor path needs the projection decl alongside
// the node decl, both on the log.
func Reaction(cfg Config) (engine.Reaction, error) {
	cfg = cfg.defaults()
	p, model, err := provider.For(cfg.Model, provider.Config{Project: cfg.Project, Location: cfg.Location})
	if err != nil {
		return nil, err
	}
	cfg.Model = model
	return body(cfg, p), nil
}

// Factory is the nodes.Factory for body kind "llm" (CONCEPT §8): it decodes the
// node's body config into an llm.Config, stamps the node name, and builds the
// body Reaction via Reaction. A composition root wires it with
// nodes.Register("llm", llm.Factory). Kept as a plain func value so this package
// does not import the registry (no import cycle; the registry imports engine
// only).
func Factory(name string, config json.RawMessage) (engine.Reaction, error) {
	var cfg Config
	if len(config) > 0 {
		if err := json.Unmarshal(config, &cfg); err != nil {
			return nil, err
		}
	}
	cfg.Name = name
	return Reaction(cfg)
}

// body is the seat's Reaction: read the history view, call the model once,
// decode the completion + function-calls into allowlisted emits, and always
// record llm.usage. A provider error becomes the body's own {node}.failed (an
// event into the cone, not a crash — doc 26 §4 / G3).
func body(cfg Config, p provider.Provider) engine.Reaction {
	allow := map[string]struct{}{}
	for _, k := range cfg.Emits {
		allow[k] = struct{}{}
	}

	return engine.ReactionFunc(func(ctx context.Context, _ engine.Event, views engine.Views) ([]engine.Emit, error) {
		// The advertised functions ARE the node's Emits — no separate tool menu
		// (doc 26 §4). Each function's parameter schema comes from the catalog by
		// kind (Views.Schema), populated dynamically (e.g. by a plugin's announced
		// kinds), so adding a tool needs no llm-body change. Built per-call so a
		// catalog grown earlier in the drain is in scope.
		var tools []provider.ToolSchema
		for _, k := range cfg.Emits {
			if k == cfg.Answer {
				continue // the answer kind is synthesised from text, not a tool
			}
			ts := provider.ToolSchema{Name: k}
			if schema, ok := views.Schema(k); ok && len(schema) > 0 {
				ts.InputSchema = schema
			}
			tools = append(tools, ts)
		}

		var system string
		var msgs []provider.Message
		if h := engine.ViewAs[History](views, cfg.HistoryName); h != nil {
			system = h.System()
			msgs = h.Messages()
		}

		resp, err := p.Complete(ctx, provider.Request{
			Model:     cfg.Model,
			System:    system,
			Messages:  msgs,
			Tools:     tools,
			MaxTokens: cfg.MaxTokens,
		})
		if err != nil {
			return nil, err // → {node}.failed
		}

		var emits []engine.Emit
		if resp.Text != "" {
			emits = append(emits, engine.Emit{
				Kind:    cfg.Answer,
				Payload: mustMarshal(map[string]string{"text": resp.Text}),
			})
		}
		for _, tc := range resp.ToolCalls {
			kind := provider.DottedToolName(tc.Name)
			if _, ok := allow[kind]; !ok {
				continue // outside the allowlist (engine also lints emit ⊆ Emits)
			}
			payload := tc.Input
			if len(payload) == 0 {
				payload = json.RawMessage("{}")
			}
			emits = append(emits, engine.Emit{Kind: kind, Payload: payload})
		}
		emits = append(emits, engine.Emit{Kind: "llm.usage", Payload: mustMarshal(resp.Usage)})
		return emits, nil
	})
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic("llm: marshal: " + err.Error())
	}
	return b
}
