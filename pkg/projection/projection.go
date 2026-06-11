// Package projection derives session state from the event log.
//
// Subscribers do not own session state; when they need to know "what has
// happened so far in this request", they call SessionProjection. This makes
// the log the single source of truth — losing or replaying it cannot
// disagree with what the system thought was happening.
package projection

import (
	"github.com/kgatilin/reflex/pkg/event"
)

// SessionState is the projected view of one request.
//
// It is rebuilt fresh from the event log on every call; nothing here is
// stored between calls. The fields are deliberately flat: this is a PoC,
// not a graph database.
type SessionState struct {
	RequestID        string
	UserMessage      string
	ToolCalls        []ToolCall
	ToolResults      []ToolResult
	AssistantOutputs []string
	Handled          bool
	Unhandled        bool
	UnhandledReason  string
	Events           []event.Event
}

// ToolCall is a request to invoke a tool, observed in the log.
type ToolCall struct {
	EventID string
	Tool    string
	Args    string
}

// ToolResult is the observed output of a tool call.
type ToolResult struct {
	EventID  string
	CausedBy string
	Result   string
}

// Event type constants used by the standard reflex handlers. Custom YAML
// configurations may emit additional event types; the projection only
// understands these.
const (
	TypeRequestReceived          = "RequestReceived"
	TypeToolCallProposed         = "ToolCallProposed"
	TypeToolResultObserved       = "ToolResultObserved"
	TypeAssistantMessageProposed = "AssistantMessageProposed"
	TypeRequestHandled           = "RequestHandled"
	TypeRequestUnhandled         = "RequestUnhandled"
	TypeEventOrphaned            = "EventOrphaned"

	// TypeLoopExhausted is the Phase 1.5 diagnostic emitted by the
	// dispatcher when a declared loop hits its max_iterations cap. It is
	// terminal: it closes the causal branch for the request_id rather
	// than spawning further work.
	TypeLoopExhausted = "LoopExhausted"

	// Phase 1.6: bus meta-events. These describe the bus's own activity
	// and are first-class events on the same log — handlers may subscribe
	// to them, the analyzer reads them, the wait-predicates key off them.
	// All three are terminal: a meta-event is an observation, never a
	// trigger for further user work.
	TypeEventDispatched = "EventDispatched"
	TypeDrainQuiesced   = "DrainQuiesced"
	TypeHandlerFailed   = "HandlerFailed"
)

// SessionProjection folds events for one request into a SessionState. Events
// belonging to other requests are ignored. The function is total: an empty
// log is a valid input.
func SessionProjection(events []event.Event, requestID string) SessionState {
	state := SessionState{RequestID: requestID}
	for _, e := range events {
		if e.RequestID != requestID {
			continue
		}
		state.Events = append(state.Events, e)
		switch e.Type {
		case TypeRequestReceived:
			var p struct {
				Payload string `json:"payload"`
			}
			_ = e.PayloadAs(&p)
			state.UserMessage = p.Payload
		case TypeToolCallProposed:
			var p struct {
				Tool string `json:"tool"`
				Args string `json:"args"`
			}
			_ = e.PayloadAs(&p)
			state.ToolCalls = append(state.ToolCalls, ToolCall{
				EventID: e.ID, Tool: p.Tool, Args: p.Args,
			})
		case TypeToolResultObserved:
			var p struct {
				Result string `json:"result"`
			}
			_ = e.PayloadAs(&p)
			state.ToolResults = append(state.ToolResults, ToolResult{
				EventID: e.ID, CausedBy: e.CausedBy, Result: p.Result,
			})
		case TypeAssistantMessageProposed:
			var p struct {
				Text string `json:"text"`
			}
			_ = e.PayloadAs(&p)
			state.AssistantOutputs = append(state.AssistantOutputs, p.Text)
		case TypeRequestHandled:
			state.Handled = true
		case TypeRequestUnhandled:
			state.Unhandled = true
			var p struct {
				Reason string `json:"reason"`
			}
			_ = e.PayloadAs(&p)
			state.UnhandledReason = p.Reason
		case TypeLoopExhausted:
			// LoopExhausted closes the request cleanly: a declared loop
			// hit its cap, dispatcher stopped firing, no orphan should
			// fire. The unhandled watcher reads Handled so we set it
			// here without emitting a synthetic RequestHandled (which
			// would lie about what happened).
			state.Handled = true
		}
	}
	return state
}

// LastToolResult returns the most recent ToolResult, if any.
func (s SessionState) LastToolResult() (ToolResult, bool) {
	if len(s.ToolResults) == 0 {
		return ToolResult{}, false
	}
	return s.ToolResults[len(s.ToolResults)-1], true
}

// LastToolCall returns the most recent ToolCall, if any.
func (s SessionState) LastToolCall() (ToolCall, bool) {
	if len(s.ToolCalls) == 0 {
		return ToolCall{}, false
	}
	return s.ToolCalls[len(s.ToolCalls)-1], true
}
