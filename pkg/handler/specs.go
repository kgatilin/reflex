package handler

import (
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/projection"
)

// llmStubSpec describes the deterministic LLM stub at the TYPE level.
//
// Consumes is "*" because the trigger event type is declared in the YAML's
// `on:` / `trigger_on:` fields, not baked into the handler. The type-level
// Emits is the maximum-possible set; the per-instance llmStubSpecResolver
// narrows it to only the actions actually declared in the config's rules +
// fallback, which keeps the static graph honest (a stub configured to only
// reply_and_handle does not appear as a ToolCallProposed emitter).
func llmStubSpec() HandlerSpec {
	return HandlerSpec{
		Type:        "llm_stub",
		Description: "deterministic LLM stub: matches rules vs. trigger event and emits tool calls or replies",
		Consumes:    "*",
		Emits: []EmittedSpec{
			{Type: projection.TypeToolCallProposed, Terminal: false, Optional: true},
			{Type: projection.TypeAssistantMessageProposed, Terminal: false, Optional: true},
			{Type: projection.TypeAssistantMessageProposed, Terminal: true, Optional: true},
			{Type: projection.TypeRequestHandled, Terminal: true, Optional: true},
		},
	}
}

// llmStubSpecResolver narrows the type-level Emits to only the actions that
// appear in the per-instance YAML config. This stops the static graph from
// hallucinating ToolCallProposed edges for stubs whose rules never call a
// tool — which would otherwise create false cycles for any
// llm_stub <-> tool_call pair (calc.yaml hits this).
func llmStubSpecResolver(cfg config.HandlerConfig, base HandlerSpec) HandlerSpec {
	hasToolCall := false
	hasReply := false
	hasReplyAndHandle := false

	scan := func(action string) {
		switch action {
		case "tool_call":
			hasToolCall = true
		case "reply":
			hasReply = true
		case "reply_and_handle":
			hasReplyAndHandle = true
		}
	}

	if cfg.Config != nil {
		if fb, ok := cfg.Config["fallback"].(map[string]any); ok {
			if a, ok := fb["action"].(string); ok {
				scan(a)
			}
		}
		if rules, ok := cfg.Config["rules"].([]any); ok {
			for _, r := range rules {
				if rm, ok := r.(map[string]any); ok {
					if a, ok := rm["action"].(string); ok {
						scan(a)
					}
				}
			}
		}
	}

	// If we couldn't introspect (no rules + no fallback declared), keep
	// the type-level maximum so the graph is still pessimistic.
	if !hasToolCall && !hasReply && !hasReplyAndHandle {
		return base
	}

	var emits []EmittedSpec
	if hasToolCall {
		emits = append(emits, EmittedSpec{Type: projection.TypeToolCallProposed, Optional: true})
	}
	if hasReply {
		emits = append(emits, EmittedSpec{Type: projection.TypeAssistantMessageProposed, Optional: true})
	}
	if hasReplyAndHandle {
		emits = append(emits, EmittedSpec{Type: projection.TypeAssistantMessageProposed, Terminal: true, Optional: true})
		emits = append(emits, EmittedSpec{Type: projection.TypeRequestHandled, Terminal: true, Optional: true})
	}
	resolved := base
	resolved.Emits = emits
	return resolved
}

func toolCallSpec() HandlerSpec {
	return HandlerSpec{
		Type:        "tool_call",
		Description: "invokes a single builtin tool against a ToolCallProposed event",
		Consumes:    projection.TypeToolCallProposed,
		Emits: []EmittedSpec{
			{Type: projection.TypeToolResultObserved, Terminal: false, Optional: false},
		},
	}
}

func printerSpec() HandlerSpec {
	return HandlerSpec{
		Type:        "printer",
		Description: "writes a field from the trigger event's payload to the configured writer",
		Consumes:    "*",
		Emits:       nil, // pure sink
	}
}

func terminatorSpec() HandlerSpec {
	return HandlerSpec{
		Type:        "terminator",
		Description: "emits RequestHandled (terminal) when its trigger fires, idempotent per request",
		Consumes:    "*",
		Emits: []EmittedSpec{
			{Type: projection.TypeRequestHandled, Terminal: true, Optional: false},
		},
	}
}

func unhandledWatcherSpec() HandlerSpec {
	return HandlerSpec{
		Type:        "unhandled_watcher",
		Description: "post-drain diagnostic: emits RequestUnhandled / EventOrphaned for stalled requests",
		Consumes:    "*",
		Emits: []EmittedSpec{
			{Type: projection.TypeRequestUnhandled, Terminal: true, Optional: true},
			{Type: projection.TypeEventOrphaned, Terminal: true, Optional: true},
		},
	}
}

func echoSpec() HandlerSpec {
	return HandlerSpec{
		Type:        "echo",
		Description: "re-emits trigger payload under a new event type taken from config.emit",
		Consumes:    "*",
		// Emits is fully dynamic — see echoSpecResolver.
		Emits: nil,
	}
}

// echoSpecResolver substitutes the configured emit type at YAML-parse time.
// Returning a default-Optional-true emission keeps the cycle detector
// pessimistic: an echo edge is treated as potentially-fire-able.
func echoSpecResolver(cfg config.HandlerConfig, base HandlerSpec) HandlerSpec {
	resolved := base
	if cfg.Config != nil {
		if v, ok := cfg.Config["emit"].(string); ok && v != "" {
			resolved.Emits = []EmittedSpec{{Type: v, Terminal: false, Optional: false}}
		}
	}
	return resolved
}

func parseTargetSpec() HandlerSpec {
	return HandlerSpec{
		Type:        "parse_target",
		Description: "parses 'owner/repo#N' (or 'repo#N') into a TargetParsed event",
		Consumes:    "*",
		Emits: []EmittedSpec{
			{Type: projection.TypeTargetParsed, Terminal: false, Optional: true},
			{Type: projection.TypeParseFailed, Terminal: true, Optional: true},
		},
	}
}

func ghQuerySpec() HandlerSpec {
	return HandlerSpec{
		Type:        "gh_query",
		Description: "shells to `gh api` for repos/{owner}/{repo}/issues/{N}/{path}; emits result or failure",
		Consumes:    "*",
		Emits: []EmittedSpec{
			{Type: projection.TypeGhQueryResult, Terminal: false, Optional: true},
			{Type: projection.TypeGhQueryFailed, Terminal: true, Optional: true},
		},
	}
}

func triageRulesSpec() HandlerSpec {
	return HandlerSpec{
		Type:        "triage_rules",
		Description: "classifies an issue as STUCK / HEALTHY / FRESH from GhQueryResult fold",
		Consumes:    "*",
		Emits: []EmittedSpec{
			{Type: projection.TypeTriageDecided, Terminal: false, Optional: true},
			{Type: projection.TypeTriagePending, Terminal: true, Optional: true},
		},
	}
}
