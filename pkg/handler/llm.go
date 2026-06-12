package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/projection"
	"github.com/kgatilin/reflex/pkg/provider"
)

// llm is the multi-model body of doc 22: one node that folds the request's
// event log into a transcript, calls the bound model ONCE through the neutral
// provider interface, and emits the completion as typed events. Unlike the
// llm_gemini + llm_decode pair it replaces, tool calls are decoded
// STRUCTURALLY — the model emits native tool_use blocks, the adapter
// translates them, and this node never parses JSON out of prose. The model is
// bound by a binding string in config (vertex:anthropic/claude-…), so
// swapping the brain is one config line (doc 22's model–role fit).
//
// Emissions per firing:
//   - one tool.<name>.call per accepted tool call (stage-0 crutch: only the
//     FIRST call is forwarded; the legacy pump topology would double-fire the
//     brain on parallel results. The surplus is reported as a terminal
//     llm.calls_dropped the model sees in its next transcript. Obligation
//     counting — self-build task #2 — retires this.)
//   - assistant.message (terminal) + RequestHandled (terminal) when the model
//     answers with text and no tool calls.
//   - llm.usage (terminal) after every successful completion — the
//     cost-tracking source of truth (tokens incl. cache, model binding,
//     stop reason).
//   - llm.failed (non-terminal) on transport/auth errors or an empty
//     completion, so the topology can react or visibly orphan.
type LLMConfig struct {
	Model     string          `json:"model"`    // binding string, e.g. vertex:anthropic/claude-opus-4-8
	Project   string          `json:"project"`  // Vertex project (ADC auth)
	Location  string          `json:"location"` // region; adapter defaults apply when empty
	System    string          `json:"system"`   // extra instructions appended to the fixed preamble
	MaxTokens int             `json:"max_tokens"`
	Tools     []LLMToolSchema `json:"tools"`
}

// LLMToolSchema is the YAML shape of one advertised tool. InputSchema is a
// full JSON Schema object; the dotted Name doubles as the emitted event kind:
// name fs.read → tool.fs.read.call.
type LLMToolSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// Event types owned by the llm node (TypeAssistantMessage, TypeLLMFailed and
// RequestHandled are shared with the legacy pair).
const (
	// TypeLLMUsage is the terminal per-call token accounting event.
	TypeLLMUsage = "llm.usage"
	// TypeLLMCallsDropped is the terminal stage-0 diagnostic for surplus
	// parallel tool calls (see the crutch note above).
	TypeLLMCallsDropped = "llm.calls_dropped"
)

// ProviderResolver resolves a model binding into a live provider. Swappable
// so tests inject a fake without touching the registry.
type ProviderResolver func(binding string, cfg provider.Config) (provider.Provider, string, error)

var (
	llmResolverMu sync.Mutex
	llmResolver   ProviderResolver = provider.For
)

// SetProviderResolver swaps the binding resolver used by every llm handler
// and returns the previous one so callers can restore it.
func SetProviderResolver(r ProviderResolver) ProviderResolver {
	llmResolverMu.Lock()
	defer llmResolverMu.Unlock()
	prev := llmResolver
	if r == nil {
		r = provider.For
	}
	llmResolver = r
	return prev
}

func currentProviderResolver() ProviderResolver {
	llmResolverMu.Lock()
	defer llmResolverMu.Unlock()
	return llmResolver
}

func newLLM(cfg config.HandlerConfig) (bus.Subscriber, error) {
	var lc LLMConfig
	if err := decodeConfig(cfg.Config, &lc); err != nil {
		return nil, fmt.Errorf("llm %q: %w", cfg.Name, err)
	}
	if lc.Model == "" {
		return nil, fmt.Errorf("llm %q: model binding is required", cfg.Name)
	}
	if _, err := provider.ParseBinding(lc.Model); err != nil {
		return nil, fmt.Errorf("llm %q: %w", cfg.Name, err)
	}

	tools := make([]provider.ToolSchema, 0, len(lc.Tools))
	for _, t := range lc.Tools {
		if t.Name == "" {
			return nil, fmt.Errorf("llm %q: every tool needs a name", cfg.Name)
		}
		schema, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("llm %q: tool %q input_schema: %w", cfg.Name, t.Name, err)
		}
		tools = append(tools, provider.ToolSchema{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		})
	}

	system := llmSystemPrompt(lc)
	name := cfg.Name
	on := cfg.On
	return &genericSub{
		baseSub: baseSub{name: name},
		on:      on,
		run: func(ctx context.Context, ev event.Event, log []event.Event) ([]event.Event, error) {
			p, model, err := currentProviderResolver()(lc.Model, provider.Config{
				Project:  lc.Project,
				Location: lc.Location,
			})
			if err != nil {
				return llmFailedEvent(err)
			}
			resp, err := p.Complete(ctx, provider.Request{
				Model:     model,
				System:    system,
				Messages:  []provider.Message{{Role: "user", Text: renderTranscript(log, ev.RequestID)}},
				Tools:     tools,
				MaxTokens: lc.MaxTokens,
			})
			if err != nil {
				return llmFailedEvent(err)
			}
			return llmEmissions(lc.Model, resp)
		},
	}, nil
}

// llmEmissions translates one neutral completion into bus events. Pure —
// unit-tested without a provider.
func llmEmissions(binding string, resp provider.Response) ([]event.Event, error) {
	usagePayload, err := json.Marshal(map[string]any{
		"model":                 binding,
		"input_tokens":          resp.Usage.InputTokens,
		"output_tokens":         resp.Usage.OutputTokens,
		"cache_read_tokens":     resp.Usage.CacheReadTokens,
		"cache_creation_tokens": resp.Usage.CacheCreationTokens,
		"stop_reason":           resp.StopReason,
	})
	if err != nil {
		return nil, err
	}
	out := []event.Event{event.NewTerminal(TypeLLMUsage, usagePayload)}

	if len(resp.ToolCalls) > 0 {
		first := resp.ToolCalls[0]
		input := first.Input
		if len(input) == 0 {
			input = json.RawMessage(`{}`)
		}
		out = append(out, event.New("tool."+first.Name+".call", input))
		if len(resp.ToolCalls) > 1 {
			var dropped []string
			for _, c := range resp.ToolCalls[1:] {
				dropped = append(dropped, c.Name)
			}
			dropPayload, err := json.Marshal(map[string]any{
				"dropped": dropped,
				"reason":  "stage-0 forwards one tool call per firing; re-issue the remaining calls one at a time",
			})
			if err != nil {
				return nil, err
			}
			out = append(out, event.NewTerminal(TypeLLMCallsDropped, dropPayload))
		}
		return out, nil
	}

	text := strings.TrimSpace(resp.Text)
	if text == "" {
		failed, err := llmFailedEvent(fmt.Errorf("empty completion (stop_reason=%s)", resp.StopReason))
		if err != nil {
			return nil, err
		}
		return append(out, failed...), nil
	}

	msgPayload, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return nil, err
	}
	out = append(out,
		event.Event{Type: TypeAssistantMessage, Payload: msgPayload, Terminal: true},
		event.Event{Type: projection.TypeRequestHandled, Terminal: true},
	)
	return out, nil
}

func llmFailedEvent(cause error) ([]event.Event, error) {
	payload, err := json.Marshal(map[string]string{"error": cause.Error()})
	if err != nil {
		return nil, err
	}
	return []event.Event{event.New(TypeLLMFailed, payload)}, nil
}

// llmSystemPrompt is the fixed contract preamble plus the per-node system
// config. Unlike the legacy llm_gemini prompt there is no JSON-action
// protocol — tool calling is native; the preamble only explains the
// event-log transcript shape and the stage-0 one-call discipline.
func llmSystemPrompt(lc LLMConfig) string {
	var b strings.Builder
	b.WriteString("You are the reasoning node inside an event-driven agent. ")
	b.WriteString("The user message is the append-only event log of one request, one `type: payload` line per event — ")
	b.WriteString("your previous tool calls and their results are in it. Decide the single next step.\n\n")
	b.WriteString("Call exactly ONE tool per turn (parallel calls are dropped and reported as llm.calls_dropped). ")
	b.WriteString("When the log already contains everything needed, answer the user in plain text with no tool call.")
	if lc.System != "" {
		b.WriteString("\n\n" + lc.System)
	}
	return b.String()
}

// llmSpec describes the multi-model body at the TYPE level. Consumes is "*"
// (the trigger is the YAML `on:`); emits are dynamic per the configured tool
// menu — see llmSpecResolver.
func llmSpec() HandlerSpec {
	return HandlerSpec{
		Type:        "llm",
		Description: "multi-model reasoning node: folds the log, calls the bound model once, emits structured tool calls / final answer / usage",
		Consumes:    "*",
		Emits:       nil,
	}
}

// llmSpecResolver derives the per-instance emit set: one tool.<name>.call
// edge per configured tool plus the fixed completion/diagnostic events. This
// keeps the static graph honest about exactly which tool kinds this node can
// emit.
func llmSpecResolver(cfg config.HandlerConfig, base HandlerSpec) HandlerSpec {
	resolved := base
	var emits []EmittedSpec
	if cfg.Config != nil {
		if tools, ok := cfg.Config["tools"].([]any); ok {
			for _, t := range tools {
				tm, ok := t.(map[string]any)
				if !ok {
					continue
				}
				if n, ok := tm["name"].(string); ok && n != "" {
					emits = append(emits, EmittedSpec{Type: "tool." + n + ".call", Optional: true})
				}
			}
		}
	}
	emits = append(emits,
		EmittedSpec{Type: TypeAssistantMessage, Terminal: true, Optional: true},
		EmittedSpec{Type: projection.TypeRequestHandled, Terminal: true, Optional: true},
		EmittedSpec{Type: TypeLLMUsage, Terminal: true, Optional: true},
		EmittedSpec{Type: TypeLLMCallsDropped, Terminal: true, Optional: true},
		EmittedSpec{Type: TypeLLMFailed, Terminal: false, Optional: true},
	)
	resolved.Emits = emits
	return resolved
}
