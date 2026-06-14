package engine

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The event catalog (doc 26 §4a): the kind→schema vocabulary, a type axis over
// the Event primitive, orthogonal to the wiring (nodes/subscriptions/
// projections, which say *who reacts*; the catalog says *what facts mean*). It
// is NOT a fourth primitive and NOT a privileged store — it is a fold (G8) over
// two population forms:
//
//   - EventKind decls (catalog.go's static/bootstrap form), seen by Validate so
//     the static checks can run before any event is appended; and
//   - event.registered facts on the log (the dynamic/runtime form), folded at
//     global horizon by the same shape — emitting one grows the catalog.
//
// Both fold into one structure. event.registered is the one PRIMORDIAL kind the
// engine knows natively (the bootstrap axiom, doc 26 §4a): it cannot register
// itself, it is the seed. Last-writer-wins on a kind tie (a later registration
// is schema evolution; "breaking-change → reject" is a later operator knob).

// seedKind is the primordial catalog kind whose schema the engine knows
// natively (doc 26 §4a). Registering any other kind is an event of THIS kind;
// this one is the axiom — it cannot register itself.
const seedKind = "event.registered"

// seedSchema is event.registered's own native schema (the schema-of-schemas
// seat, doc 26 §4a). A registration carries {kind: string, schema: any}; schema
// is left unconstrained (it is itself a JSON Schema document, which our
// lightweight subset does not model). This is the only schema not folded from
// the log — it is the seed the catalog is built from.
var seedSchema = json.RawMessage(`{` +
	`"type":"object",` +
	`"properties":{"kind":{"type":"string"},"schema":{"type":"object"}},` +
	`"required":["kind"]` +
	`}`)

// catalog is the materialised kind→schema fold. A nil/absent Schema entry means
// "kind is known, no payload constraint" (doc 26 §4a: a registered kind without
// a declared schema is still valid/advertisable). empty reports whether any
// catalog entry was declared at all — the opt-in-dormancy switch (the static
// checks gate Connected only over a NON-empty catalog; see validate.go).
type catalog struct {
	schemas map[string]json.RawMessage
	// declared counts only operator-supplied entries (EventKind decls +
	// event.registered facts), NOT the engine's primordial seed kind. The seed
	// is always present so a topology can subscribe to / register kinds, but it
	// must not by itself flip the catalog from "absent" to "present" — otherwise
	// the opt-in-until-adopted rule (doc 26 §4a / stage 2d) would never hold and
	// every existing catalog-less topology would suddenly be gated.
	declared int
}

// foldCatalog builds the catalog from the static EventKind decls plus any
// event.registered facts on the log (doc 26 §4a). The seed kind is always
// installed (the bootstrap axiom); operator entries fold over it in order, so a
// later registration of the same kind wins (last-writer-wins). The log is
// optional — Validate passes nil (it has no log; the static form is decls only)
// and the runtime path passes Events so dynamic registrations are reflected.
//
// It stays payload-blind in spirit: it reads only an event.registered payload's
// two declared fields (kind, schema) mechanically to grow the type layer; it
// never interprets a domain payload. The fold itself is the catalog's whole
// definition (caches are strategies, the fold is the truth — G1/G8).
func foldCatalog(decls []Decl, log []Event) catalog {
	c := catalog{schemas: map[string]json.RawMessage{}}
	c.schemas[seedKind] = seedSchema // the axiom, always known

	for _, d := range decls {
		ek, ok := d.(EventKind)
		if !ok || ek.Kind == "" {
			continue
		}
		c.schemas[ek.Kind] = ek.Schema
		c.declared++
	}

	for _, ev := range log {
		_, _, kind := splitSubject(ev.Subject)
		if kind != seedKind {
			continue
		}
		regKind, regSchema, ok := parseRegistration(ev.Payload)
		if !ok || regKind == "" {
			continue
		}
		c.schemas[regKind] = regSchema
		c.declared++
	}
	return c
}

// empty reports whether the catalog carries no operator-declared kinds — only
// the primordial seed. When empty, the static catalog checks are DORMANT (doc
// 26 §4a opt-in-until-adopted): an unadopted catalog must not fail every
// existing topology. The checks gate Connected only once the catalog is grown.
func (c catalog) empty() bool { return c.declared == 0 }

// has reports whether a concrete kind is in the catalog.
func (c catalog) has(kind string) bool {
	_, ok := c.schemas[kind]
	return ok
}

// kinds returns the catalog's concrete kinds, including the seed. Used by the
// dead-subscription check to test whether an On pattern matches ≥1 catalog kind.
func (c catalog) kinds() []string {
	out := make([]string, 0, len(c.schemas))
	for k := range c.schemas {
		out = append(out, k)
	}
	return out
}

// schemaOf returns the declared schema for a kind, and whether the kind is in
// the catalog at all. A known kind with a nil/empty schema returns (nil, true):
// known, but no payload constraint.
func (c catalog) schemaOf(kind string) (json.RawMessage, bool) {
	s, ok := c.schemas[kind]
	return s, ok
}

// registration is the event.registered payload shape (doc 26 §4a): {kind,
// schema}. Exported as a helper for tests and the runtime registration path so
// a caller does not hand-roll the seed payload.
type registration struct {
	Kind   string          `json:"kind"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

// parseRegistration reads an event.registered payload's two declared fields
// mechanically. A malformed payload yields ok=false (the registration is
// ignored — it never poisons the fold; a non-conforming event.registered would
// itself be caught by payload-conformance against the seed schema).
func parseRegistration(payload json.RawMessage) (kind string, schema json.RawMessage, ok bool) {
	var r registration
	if err := json.Unmarshal(payload, &r); err != nil {
		return "", nil, false
	}
	return r.Kind, r.Schema, true
}

// RegisterPayload builds the payload of an event.registered emit (doc 26 §4a):
// the kind being registered and its schema. It is the convenience for the
// runtime/dynamic registration path so a caller emits a well-formed seed fact.
func RegisterPayload(kind string, schema json.RawMessage) json.RawMessage {
	return mustMarshal(registration{Kind: kind, Schema: schema})
}

// conforms runs the lightweight payload-conformance check (doc 26 §4a runtime
// half) of a concrete payload against a kind's catalog schema. It returns nil
// when the payload conforms (or when the kind has no declared schema), and a
// descriptive error otherwise — the caller turns a non-nil error into the
// body's own .failed (doc 26 §4 / G3), never a panic.
//
// SUPPORTED JSON-SCHEMA SUBSET (deliberately minimal — doc 26 §4a says a full
// implementation is not needed; a structural check is enough):
//
//   - "type" at the top level: only "object" is structurally enforced (the
//     payload must be a JSON object). Other top-level types are accepted as
//     conforming without deeper checks (a permissive default — we do not model
//     arrays/strings/numbers as top-level event payloads here).
//   - "required": [..] — each named property must be present in the payload.
//   - "properties": {name: {"type": t}} — for a present property, its JSON value
//     must match the declared scalar type t. Recognised t: "string", "number"
//     (incl. integer JSON numbers), "integer", "boolean", "object", "array".
//     An unknown/absent declared type is not enforced (permissive). Nested
//     sub-schemas beyond "type" are NOT validated (one level only).
//
// NOT supported (documented gaps, future knobs): enum, pattern, format, minimum/
// maximum, additionalProperties, nested object/array schemas, oneOf/anyOf/allOf,
// $ref. These never reject — the subset errs permissive so a kind can register a
// rich schema and only the structural core is enforced at runtime.
func conforms(payload json.RawMessage, schema json.RawMessage) error {
	if len(schema) == 0 {
		return nil // known kind, no declared constraint
	}
	var s schemaDoc
	if err := json.Unmarshal(schema, &s); err != nil {
		// A schema the engine cannot parse imposes no constraint — it is a
		// registration concern, not a payload fault. Stay permissive.
		return nil
	}
	if s.Type != "" && s.Type != "object" {
		return nil // only object is structurally enforced (see subset doc)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil {
		// Schema expects (or defaults to) an object; the payload is not one.
		return fmt.Errorf("payload is not a JSON object as the schema requires")
	}

	for _, req := range s.Required {
		if _, present := obj[req]; !present {
			return fmt.Errorf("missing required field %q", req)
		}
	}

	for name, prop := range s.Properties {
		val, present := obj[name]
		if !present {
			continue // presence is enforced by "required", not "properties"
		}
		if prop.Type == "" {
			continue // no declared type → not enforced
		}
		if !jsonValueIsType(val, prop.Type) {
			return fmt.Errorf("field %q must be %s", name, prop.Type)
		}
	}
	return nil
}

// schemaDoc is the one-level JSON Schema shape the subset above reads. Anything
// not named here is ignored — the check is structural, not a full validator.
type schemaDoc struct {
	Type       string                `json:"type"`
	Required   []string              `json:"required"`
	Properties map[string]schemaProp `json:"properties"`
}

type schemaProp struct {
	Type string `json:"type"`
}

// jsonValueIsType reports whether a raw JSON value matches a declared scalar
// type. It inspects the value's first significant byte (JSON's grammar makes the
// kind unambiguous from it) plus a numeric distinction for integer.
func jsonValueIsType(raw json.RawMessage, typ string) bool {
	t := strings.TrimSpace(string(raw))
	if t == "" {
		return false
	}
	switch typ {
	case "string":
		return t[0] == '"'
	case "boolean":
		return t == "true" || t == "false"
	case "object":
		return t[0] == '{'
	case "array":
		return t[0] == '['
	case "number":
		return isJSONNumber(t)
	case "integer":
		if !isJSONNumber(t) {
			return false
		}
		// An integer must have no fractional/exponent part.
		return !strings.ContainsAny(t, ".eE")
	default:
		return true // unknown declared type → permissive
	}
}

// isJSONNumber reports whether t parses as a JSON number.
func isJSONNumber(t string) bool {
	var n json.Number
	if err := json.Unmarshal([]byte(t), &n); err != nil {
		return false
	}
	return true
}
