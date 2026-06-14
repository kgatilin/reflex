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

// Frame is the single envelope every wire message decodes into; Type selects
// which fields are meaningful. snake_case matches the event JSON convention.
type Frame struct {
	Type     string `json:"type"`
	Protocol int    `json:"protocol,omitempty"` // hello
	Name     string `json:"name,omitempty"`     // hello
	ID       string `json:"id,omitempty"`       // invoke / result (correlator)
	Event    *Event `json:"event,omitempty"`    // invoke
	Emits    []Emit `json:"emits,omitempty"`    // result
	Error    string `json:"error,omitempty"`    // result
}
