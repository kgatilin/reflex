// Package llm is the reasoning body (docs/24-concept.md §3): one of the
// two reaction bodies in the user vocabulary. It folds its declared
// views, calls the bound model once, and emits typed actions from an
// allowlist. It holds no state and decides nothing about wiring — "the
// brain" and "a medic" differ only in subscriptions plus config.
package llm

import (
	"github.com/kgatilin/reflex/engine"
)

// Config parameterizes the body. Per §A.4 this is behaviour, not wiring:
// the direction of travel is config-as-log-facts read through a view;
// the struct form is the bootstrap step on that path.
type Config struct {
	// Model is a provider binding, e.g. "vertex:gemini-3.5-flash" —
	// multi-model is config, one line per seat (§9).
	Model    string
	Project  string
	Location string

	System    string
	MaxTokens int

	// Transcript names the log-shaped view folded into the conversation.
	// The transcript is a declared projection like any other — the body
	// does not read the raw log.
	Transcript string

	// Menu names the kv view advertising callable tools. The menu is a
	// projection of the tool.*.call consumers (§3): register a plugin,
	// the model gains the tool; no per-node tool config.
	Menu string

	// Actions is the emit allowlist: the kinds this seat may produce.
	// Text completions and tool calls both decode into allowlisted Emits;
	// anything outside the list is the body's own failed event.
	Actions []string
}

// New builds the body as an engine.Reaction. The model transport behind
// it is pkg/provider — the neutral completion interface and its three
// Vertex adapters are reused from stage 0 unchanged.
func New(cfg Config) (engine.Reaction, error) {
	panic("llm: not implemented — skeleton for API review")
}
