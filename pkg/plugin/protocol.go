// Package plugin is the reflex out-of-process plugin seam (doc 29 Iteration 3).
//
// A plugin is a child process the daemon drives over a byte stream — stdio for
// now (the daemon execs the plugin and talks over its stdin/stdout); a socket
// transport drops in later behind the same protocol. The wire is
// newline-delimited JSON (NDJSON): one Frame per line, framing stays trivial.
//
// This package is deliberately engine-free. The host side is Client (drive a
// plugin); the plugin-author side is Serve (one call in a plugin's main). The
// engine adapter — turning a Client into an engine.Reaction — lives in
// nodes/proxy so this package stays a clean, portable protocol reference (a
// plugin can be written in any language that speaks the same NDJSON).
package plugin

import "encoding/json"

// Protocol is the wire version, exchanged in the hello/welcome handshake so a
// mismatch fails fast rather than producing baffling decode errors downstream.
const Protocol = 1

// Message types — the Frame.Type discriminator, one token each.
const (
	TypeHello   = "hello"   // plugin -> host, on start: announces name + protocol
	TypeWelcome = "welcome" // host -> plugin: handshake accepted
	TypeInvoke  = "invoke"  // host -> plugin: react to one event
	TypeResult  = "result"  // plugin -> host: the produced emits (or an error)
)

// Event is the wire form of the triggering event handed to a plugin: the full
// subject plus the opaque payload. Views are deliberately NOT crossed over the
// wire — hands react to the event, not to in-process projections (those are the
// brain's surface).
type Event struct {
	Subject string          `json:"subject"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Emit is the wire form of one produced event — a kind tail + payload, mirroring
// engine.Emit. The engine still binds these to the node's declared emit set: a
// plugin cannot emit outside what its node was wired to emit (out-of-process is
// not out-of-bounds).
type Emit struct {
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Role is the direction of a declared event relative to the plugin: In kinds it
// consumes (its subscription), Out kinds it emits.
type Role string

const (
	RoleIn  Role = "in"
	RoleOut Role = "out"
)

// EventDecl is one catalog entry a plugin announces in its hello: a kind, its
// JSON schema (the LLM-tool-compatible subset for In kinds — what the llm body
// advertises as a function schema), and the direction. The daemon turns these
// into the node's On/Emits wiring AND into sys.event.registered facts, so the
// catalog is populated dynamically at connect time, never hardcoded in the host.
// Schema may be empty (a known kind with no payload constraint).
type EventDecl struct {
	Kind   string          `json:"kind"`
	Schema json.RawMessage `json:"schema,omitempty"`
	Role   Role            `json:"role"`
}

// Spec is the plugin's full self-description, announced in hello.
type Spec struct {
	Name   string      `json:"name"`
	Events []EventDecl `json:"events,omitempty"`
}

// In returns the kinds this spec consumes (role=in); Out the kinds it emits.
func (s Spec) In() []string  { return s.kinds(RoleIn) }
func (s Spec) Out() []string { return s.kinds(RoleOut) }

func (s Spec) kinds(role Role) []string {
	var out []string
	for _, e := range s.Events {
		if e.Role == role {
			out = append(out, e.Kind)
		}
	}
	return out
}

// Frame is the single envelope every wire message decodes into; Type selects
// which fields are meaningful. snake_case matches the event JSON convention.
type Frame struct {
	Type     string      `json:"type"`
	Protocol int         `json:"protocol,omitempty"` // hello
	Name     string      `json:"name,omitempty"`     // hello
	Events   []EventDecl `json:"events,omitempty"`   // hello (self-description)
	ID       string      `json:"id,omitempty"`       // invoke / result (correlator)
	Event    *Event      `json:"event,omitempty"`    // invoke
	Emits    []Emit      `json:"emits,omitempty"`    // result
	Error    string      `json:"error,omitempty"`    // result
}
