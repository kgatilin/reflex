// Package topology is the declarative topology document — the human/client
// format a daemon API request or a YAML file carries, decoded into engine.Decls
// (doc 20 / CONCEPT §8). It is the public schema of the control plane: the
// engine's on-log fact specs (changeset.go) stay internal; this is what an
// operator writes and what crosses the daemon's API. A node names a body by
// kind + config (the serializable descriptor, resolver.go) — never a Go closure
// — so a document is fully applicable through engine.Apply with a body resolver.
package topology

import (
	"encoding/json"
	"fmt"

	"github.com/kgatilin/reflex/engine"
	"gopkg.in/yaml.v3"
)

// Document is a whole topology (or a changeset fragment): scopes, subscribers,
// projections, and event-catalog entries. Applied as one changeset, it is
// validated as a resulting graph (the engine rejects a disconnected outcome).
type Document struct {
	Scopes      []ScopeSpec      `json:"scopes,omitempty" yaml:"scopes,omitempty"`
	Subscribers []SubscriberSpec `json:"subscribers,omitempty" yaml:"subscribers,omitempty"`
	Projections []ProjectionSpec `json:"projections,omitempty" yaml:"projections,omitempty"`
	Events      []EventSpec      `json:"events,omitempty" yaml:"events,omitempty"`
}

// SubscriberSpec is a subscriber: its subscription/scope/emit wiring plus a body
// descriptor. A subscriber with an empty Body.Kind is a sink (consumes, emits
// nothing).
type SubscriberSpec struct {
	Name  string   `json:"name" yaml:"name"`
	On    []string `json:"on,omitempty" yaml:"on,omitempty"`
	In    string   `json:"in,omitempty" yaml:"in,omitempty"`
	Reads []string `json:"reads,omitempty" yaml:"reads,omitempty"`
	Emits []string `json:"emits,omitempty" yaml:"emits,omitempty"`
	Scope string   `json:"scope,omitempty" yaml:"scope,omitempty"`
	Body  BodySpec `json:"body,omitzero" yaml:"body,omitempty"`
}

// BodySpec is the serializable body descriptor: a kind resolved by the engine's
// BodyResolver (e.g. "llm") and its opaque config. Empty Kind ⇒ a sink.
type BodySpec struct {
	Kind   string `json:"kind,omitempty" yaml:"kind,omitempty"`
	Config any    `json:"config,omitempty" yaml:"config,omitempty"`
}

// ScopeSpec declares a kind-rooted scope and its optional per-kind budget.
type ScopeSpec struct {
	Name   string         `json:"name" yaml:"name"`
	Root   string         `json:"root,omitempty" yaml:"root,omitempty"`
	Budget map[string]int `json:"budget,omitempty" yaml:"budget,omitempty"`
}

// ProjectionSpec declares a view: its fold patterns, horizon, type, and the
// selectors/params the type's builder reads.
type ProjectionSpec struct {
	Name   string   `json:"name" yaml:"name"`
	On     []string `json:"on,omitempty" yaml:"on,omitempty"`
	In     string   `json:"in,omitempty" yaml:"in,omitempty"`
	Type   string   `json:"type,omitempty" yaml:"type,omitempty"`
	Key    string   `json:"key,omitempty" yaml:"key,omitempty"`
	Value  string   `json:"value,omitempty" yaml:"value,omitempty"`
	Params any      `json:"params,omitempty" yaml:"params,omitempty"`
}

// EventSpec registers one catalog kind and its (optional) payload schema.
type EventSpec struct {
	Kind   string `json:"kind" yaml:"kind"`
	Schema any    `json:"schema,omitempty" yaml:"schema,omitempty"`
	// Terminal marks the kind a declared leaf — a graph output (consumed by a
	// client, e.g. request.terminal) or an observability fact — so the validator
	// does not require an in-graph consumer for it (engine.EventKind.Terminal).
	Terminal bool `json:"terminal,omitempty" yaml:"terminal,omitempty"`
}

// Parse decodes a YAML or JSON document (YAML is a superset of JSON, so one
// decoder serves both) into a Document.
func Parse(data []byte) (Document, error) {
	var d Document
	if err := yaml.Unmarshal(data, &d); err != nil {
		return Document{}, fmt.Errorf("topo: parse: %w", err)
	}
	return d, nil
}

// Decls turns the document into engine declarations, in the order scopes →
// nodes → projections → events (the engine folds them, so order is for
// determinism only). Opaque config/params/schema are re-marshalled to JSON for
// the engine's payload-blind layers.
func (d Document) Decls() ([]engine.Decl, error) {
	var out []engine.Decl
	for _, s := range d.Scopes {
		out = append(out, engine.Scope{Name: s.Name, Root: s.Root, Budget: s.Budget})
	}
	for _, n := range d.Subscribers {
		cfg, err := toRaw(n.Body.Config)
		if err != nil {
			return nil, fmt.Errorf("topo: node %q body config: %w", n.Name, err)
		}
		out = append(out, engine.Subscriber{
			Name: n.Name, On: n.On, In: n.In, Reads: n.Reads, Emits: n.Emits, Scope: n.Scope,
			BodyKind: n.Body.Kind, BodyConfig: cfg,
		})
	}
	for _, p := range d.Projections {
		params, err := toRaw(p.Params)
		if err != nil {
			return nil, fmt.Errorf("topo: projection %q params: %w", p.Name, err)
		}
		out = append(out, engine.Projection{
			Name: p.Name, On: p.On, In: engine.Horizon(p.In), Type: p.Type,
			Key: p.Key, Value: p.Value, Params: params,
		})
	}
	for _, e := range d.Events {
		schema, err := toRaw(e.Schema)
		if err != nil {
			return nil, fmt.Errorf("topo: event %q schema: %w", e.Kind, err)
		}
		out = append(out, engine.EventKind{Kind: e.Kind, Schema: schema, Terminal: e.Terminal})
	}
	return out, nil
}

// toRaw re-marshals an opaque decoded value (from YAML or JSON) into JSON bytes
// the engine carries verbatim. A nil value yields nil (no payload constraint /
// no config).
func toRaw(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// FromDecls renders live engine decls back into a Document — the read side
// (`topo show`). Bodies render as their descriptor (kind + config); a live
// in-process closure has no descriptor and renders as a bare kind so the shape
// is still legible.
func FromDecls(decls []engine.Decl) Document {
	var d Document
	for _, decl := range decls {
		switch v := decl.(type) {
		case engine.Scope:
			d.Scopes = append(d.Scopes, ScopeSpec{Name: v.Name, Root: v.Root, Budget: v.Budget})
		case engine.Subscriber:
			d.Subscribers = append(d.Subscribers, SubscriberSpec{
				Name: v.Name, On: v.On, In: v.In, Reads: v.Reads, Emits: v.Emits, Scope: v.Scope,
				Body: BodySpec{Kind: v.BodyKind, Config: rawToAny(v.BodyConfig)},
			})
		case engine.Projection:
			d.Projections = append(d.Projections, ProjectionSpec{
				Name: v.Name, On: v.On, In: string(v.In), Type: v.Type,
				Key: v.Key, Value: v.Value, Params: rawToAny(v.Params),
			})
		case engine.EventKind:
			d.Events = append(d.Events, EventSpec{Kind: v.Kind, Schema: rawToAny(v.Schema)})
		}
	}
	return d
}

// rawToAny decodes JSON bytes back into a generic value for re-rendering. Invalid
// or empty bytes render as nil (omitted).
func rawToAny(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}
