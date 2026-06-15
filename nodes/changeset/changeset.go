// Package changeset is the bridge body (body kind "changeset") that turns a
// topology DOCUMENT into an in-graph topology changeset request. It is the seam
// an agent uses to BUILD topology at runtime without the engine ever knowing the
// document format: a reasoning node (an llm brain) emits a friendly "apply this
// topology" tool-call carrying a YAML/JSON document under a payload field; this
// body parses it (pkg/topology, the same format an operator file or the daemon
// API uses), turns it into changeset ops, and emits engine.KindChangesetRequested.
// The engine's in-graph dispatch hook then validates and commits it exactly as an
// operator Apply would (engine.commitChangeset), and a subgraph the document adds
// goes live in the same drain (engine live-refresh).
//
// The brain thus has a single high-level capability — "here is the topology I
// want" — and the kernel keeps its narrow ops vocabulary. The document is the
// human/operator format; this body is the only place it is parsed on the in-graph
// path, mirroring pkg/daemon's readDoc on the API path.
//
// Config (BodyConfig), all optional:
//
//	emit:      the changeset-request kind to emit (default engine.KindChangesetRequested).
//	fail:      the kind emitted on a parse/decls error, carrying {error} (default
//	           "topology.apply.failed") — the brain subscribes to it for feedback,
//	           the symmetric companion of engine's changeset.rejected.
//	principal: the principal tag stamped on the changeset request (default "in-graph").
//	field:     the trigger payload field holding the document (default "document").
//	           The field value may be a YAML/JSON string OR an inline JSON object.
//
// Both emit and fail must be in the subscriber's Emits allowlist.
package changeset

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kgatilin/reflex/engine"
	"github.com/kgatilin/reflex/pkg/topology"
)

// Config is the changeset bridge descriptor (all fields optional; see package doc).
type Config struct {
	Emit      string `json:"emit,omitempty"`
	Fail      string `json:"fail,omitempty"`
	Principal string `json:"principal,omitempty"`
	Field     string `json:"field,omitempty"`
}

const (
	defaultFail  = "topology.apply.failed"
	defaultField = "document"
	defaultPrinc = "in-graph"
)

// Catalog self-describes the bridge's tool (a nodes.Describer): it registers the
// kind(s) the bridge HANDLES — the subscriber's On — each carrying the {document}
// parameter schema a reasoning node advertises, so the brain emits a document and
// not an empty call; plus the FAIL kind it emits (registered so a subscription to
// it is not dead). The changeset-request kind it emits is engine-owned
// (self-registered there). This is the in-process parallel of a plugin announcing
// its tool schema — an operator topology wires the bridge WITHOUT re-declaring the
// schema. Register it with nodes.RegisterCatalog("changeset", changeset.Catalog).
func Catalog(s engine.Subscriber) []engine.EventKind {
	var cfg Config
	if len(s.BodyConfig) > 0 {
		_ = json.Unmarshal(s.BodyConfig, &cfg)
	}
	fail := cfg.Fail
	if fail == "" {
		fail = defaultFail
	}
	field := cfg.Field
	if field == "" {
		field = defaultField
	}
	schema := json.RawMessage(fmt.Sprintf(
		`{"type":"object","properties":{%q:{"type":"string","description":"A reflex topology document (YAML) to apply — the worker subgraph to build or extend."}},"required":[%q]}`,
		field, field))

	out := []engine.EventKind{{Kind: fail}} // bridge → brain: parse-failure feedback
	for _, k := range s.On {                // the apply-topology tool(s) the brain calls
		out = append(out, engine.EventKind{Kind: k, Schema: schema})
	}
	return out
}

// Factory is the nodes.Factory for body kind "changeset". Register it with
// nodes.Register("changeset", changeset.Factory).
func Factory(s engine.Subscriber) (engine.Reaction, error) {
	var cfg Config
	if len(s.BodyConfig) > 0 {
		if err := json.Unmarshal(s.BodyConfig, &cfg); err != nil {
			return nil, fmt.Errorf("changeset: body config: %w", err)
		}
	}
	emit := cfg.Emit
	if emit == "" {
		emit = engine.KindChangesetRequested
	}
	fail := cfg.Fail
	if fail == "" {
		fail = defaultFail
	}
	principal := cfg.Principal
	if principal == "" {
		principal = defaultPrinc
	}
	field := cfg.Field
	if field == "" {
		field = defaultField
	}

	return engine.ReactionFunc(func(_ context.Context, ev engine.Event, _ engine.Views) ([]engine.Emit, error) {
		data, err := documentBytes(ev.Payload, field)
		if err != nil {
			return []engine.Emit{failEmit(fail, err)}, nil
		}
		doc, err := topology.Parse(data)
		if err != nil {
			return []engine.Emit{failEmit(fail, err)}, nil
		}
		decls, err := doc.Decls()
		if err != nil {
			return []engine.Emit{failEmit(fail, err)}, nil
		}
		return []engine.Emit{{
			Kind:    emit,
			Payload: engine.ChangesetRequestPayload(decls, principal),
		}}, nil
	}), nil
}

// documentBytes extracts the document from the trigger payload's named field. The
// field value may be a YAML/JSON STRING (the natural shape from a function-calling
// model that writes YAML) or an inline JSON OBJECT (already structured); either is
// returned as bytes topology.Parse accepts (YAML is a JSON superset).
func documentBytes(payload json.RawMessage, field string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil {
		return nil, fmt.Errorf("trigger payload is not a JSON object: %w", err)
	}
	raw, ok := obj[field]
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("trigger payload has no %q field carrying the topology document", field)
	}
	// A JSON string field → unquote to its raw text (YAML or JSON). Anything else
	// (an object/array) is passed through verbatim for topology.Parse.
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("document field %q is a malformed string: %w", field, err)
		}
		return []byte(s), nil
	}
	return raw, nil
}

// failEmit builds the parse-failure feedback event carrying the error text.
func failEmit(kind string, err error) engine.Emit {
	p, _ := json.Marshal(map[string]string{"error": err.Error()})
	return engine.Emit{Kind: kind, Payload: p}
}
