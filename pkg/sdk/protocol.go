// Package sdk is the reflex handler client library.
//
// It lets external processes — or in-process tests — register a handler with
// the reflex bus and react to events as if the handler were YAML-declared.
// The SDK supports two transports:
//
//   - InProcess: a thin adapter over an existing *bus.Bus. The handler runs
//     in the same address space as the bus; there is no network involved.
//     Used by `reflex run` and by integration tests.
//
//   - Remote: a connection to a `reflex daemon` over a Unix domain socket.
//     The handler runs in its own process; events flow over the socket.
//
// Phase 4a wire protocol — newline-delimited JSON (NDJSON) over a single
// duplex Unix-socket connection.
//
// Why NDJSON: one line per message keeps framing trivial (bufio.Scanner with
// a generous buffer), the messages are small (a single event payload plus a
// handful of metadata fields), and it is trivial to eyeball / tcpdump.
// Length-prefixed framing would add zero benefit at this scale and one more
// thing to get wrong.
//
// Message envelope: every line is a JSON object whose `kind` field
// discriminates the message type. Field names are lowercase_snake_case to
// match the event JSON convention.
//
// Direction client→daemon:
//
//	{"kind":"hello","handler":{"name":"my-h","consumes":"X","emits":[...]}}
//	{"kind":"emit","delivery_id":"…","event":{...event.Event...}}   (during delivery — collected by React)
//	{"kind":"emit","event":{...event.Event...}}                     (no delivery_id — treated as a fresh seed)
//	{"kind":"ack","delivery_id":"…"}                                (handler done with event)
//	{"kind":"nack","delivery_id":"…","error":"…"}                   (handler errored)
//	{"kind":"goodbye"}
//
// Direction daemon→client:
//
//	{"kind":"welcome","handler_name":"my-h"}
//	{"kind":"deliver","delivery_id":"…","event":{...}}
//	{"kind":"error","error":"…"}                       (fatal protocol error)
//
// Heartbeats: not yet — the daemon is single-process and a dead socket is
// detected by read/write errors. A heartbeat layer can be added later
// without breaking the protocol (new `kind` value).
//
// Out of scope here (TODO(phase-4b/4c/4d)): treating subscription as an
// event on the bus, permission/scope checks, multi-handler-per-connection
// muxing. One connection = one handler in Phase 4a.
package sdk

import (
	"encoding/json"

	"github.com/kgatilin/reflex/pkg/event"
)

// ProtocolVersion is bumped whenever an incompatible change lands. Clients
// and the daemon exchange this in hello/welcome so a mismatch fails fast
// rather than producing baffling decode errors.
const ProtocolVersion = 1

// Message kinds. Lowercase, single token — they appear on the wire as the
// `kind` field of every JSON line.
const (
	KindHello   = "hello"
	KindWelcome = "welcome"
	KindDeliver = "deliver"
	KindAck     = "ack"
	KindNack    = "nack"
	KindEmit    = "emit"
	KindGoodbye = "goodbye"
	KindError   = "error"

	// Phase 4b additions.

	// KindAwait is sent client→daemon to subscribe to a wait predicate
	// (drain | request_id_terminal | projection.has=<key>) for a given
	// request_id. The daemon resolves the predicate after each drain
	// (or after a deadline) and replies with KindResolved / KindTimeout.
	KindAwait    = "await"
	KindResolved = "resolved"
	KindTimeout  = "timeout"

	// Projection RPCs for remote handlers (KindProjSet / KindProjGet /
	// KindProjValue). Get is request/reply; Set is fire-and-forget.
	KindProjSet   = "proj_set"
	KindProjGet   = "proj_get"
	KindProjValue = "proj_value"
)

// Frame is the envelope every wire message decodes into. Unknown fields are
// preserved as raw JSON in the kind-specific sub-fields below; this means
// adding a new kind on the daemon side doesn't break old clients catastrophically.
type Frame struct {
	Kind    string `json:"kind"`
	Version int    `json:"version,omitempty"`

	// Hello / Welcome
	Handler     *HandlerSpec `json:"handler,omitempty"`
	HandlerName string       `json:"handler_name,omitempty"`

	// Deliver / Ack / Nack
	DeliveryID string       `json:"delivery_id,omitempty"`
	Event      *event.Event `json:"event,omitempty"`

	// Error / Nack
	Error string `json:"error,omitempty"`

	// Await / Resolved / Timeout (Phase 4b). AwaitID lets the daemon
	// correlate multiple in-flight predicates from the same CLI client.
	AwaitID   string `json:"await_id,omitempty"`
	Predicate string `json:"predicate,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	Reason    string `json:"reason,omitempty"`

	// Projection RPCs (Phase 4b). RPCID is the request/reply correlator
	// for proj_get / proj_value. Key/Value carry the payload; Found
	// reports whether the key existed (proj_value only).
	RPCID string          `json:"rpc_id,omitempty"`
	Key   string          `json:"key,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
	Found bool            `json:"found,omitempty"`
}

// HandlerSpec is the wire description of the handler the client is
// registering. It mirrors the YAML handler config closely: consumes one
// event type, may emit some, may declare some emissions terminal.
type HandlerSpec struct {
	Name     string        `json:"name"`
	Consumes string        `json:"consumes"`
	Emits    []EmittedSpec `json:"emits,omitempty"`
}

// EmittedSpec mirrors handler.EmittedSpec on the wire. It is kept here as a
// separate type so the SDK does not import the handler package (which would
// create a layering cycle: bus depends on nothing, sdk would depend on bus
// + handler, and handler tests depend on bus).
type EmittedSpec struct {
	Type     string `json:"type"`
	Terminal bool   `json:"terminal,omitempty"`
	Optional bool   `json:"optional,omitempty"`
}

// EncodeFrame marshals a frame to a single line (no trailing newline — the
// transport adds it).
func EncodeFrame(f Frame) ([]byte, error) {
	return json.Marshal(f)
}

// DecodeFrame parses one wire line into a Frame.
func DecodeFrame(line []byte) (Frame, error) {
	var f Frame
	err := json.Unmarshal(line, &f)
	return f, err
}
