// Package provider is the multi-model seam of the llm body (doc 22): one
// neutral completion interface, per-backend adapters behind it. A node binds
// a model with a string — "vertex:anthropic/claude-opus-4-8" — and everything
// provider-specific (auth, wire dialect, tool-call encoding) stays inside the
// adapter. Events on the bus never carry provider dialect; the log is
// provider-neutral, which is what makes cassette replay / model A/B free.
//
// Stage 0 ships exactly one adapter: Vertex-Anthropic (the agent's hands must
// aim precisely). The Gemini and OpenAI-compatible adapters are deliberately
// NOT here — they are the bootstrap agent's first calibration task.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Message is one turn of provider-neutral conversation input. Stage 0 sends a
// single user message containing the folded transcript; the type still
// carries a role so richer foldings need no interface change.
type Message struct {
	Role string // "user" | "assistant"
	Text string
}

// ToolSchema advertises one callable tool to the model. InputSchema is a full
// JSON Schema object ({"type":"object","properties":{...},"required":[...]});
// adapters translate it into their wire dialect.
//
// Name may contain dots (the subject segments of the tool kind, e.g.
// "fs.read" → tool.fs.read.call). Adapters whose wire format forbids dots
// (Anthropic: ^[a-zA-Z0-9_-]+$) transcode dots to underscores on the way out
// and back on the way in — which is bijective only while individual segments
// stay underscore-free. Keep tool segments like fs, read, go, build.
type ToolSchema struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// ToolCall is one structured tool invocation decoded from the completion.
// Input is the model-produced argument object, verbatim JSON.
type ToolCall struct {
	ID    string
	Name  string // dotted form, matching ToolSchema.Name
	Input json.RawMessage
}

// Usage is the per-call token accounting every adapter must report — the
// source of truth for cost tracking. Cache fields are zero on backends
// without prompt caching.
type Usage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
}

// Request is one completion call: system + messages + tool menu in, exactly
// one model invocation out.
type Request struct {
	Model     string // adapter-local model id (binding already stripped)
	System    string
	Messages  []Message
	Tools     []ToolSchema
	MaxTokens int
}

// Response is the neutral completion: assistant text and/or tool calls, the
// stop reason verbatim, and token usage.
type Response struct {
	Text       string
	ToolCalls  []ToolCall
	StopReason string
	Usage      Usage
}

// Provider is the one interface every backend adapter implements:
// (messages, tool schemas) → completion | tool calls.
type Provider interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// Binding is a parsed model-binding string. The grammar is
// "<transport>:<family>/<model>" with family optional:
//
//	vertex:anthropic/claude-opus-4-8   → {Transport: vertex, Family: anthropic, Model: claude-opus-4-8}
//	vertex:gemini-2.5-pro              → {Transport: vertex, Family: "", Model: gemini-2.5-pro}
//	vertex:deepseek/deepseek-r1        → {Transport: vertex, Family: deepseek, Model: deepseek-r1}
type Binding struct {
	Transport string
	Family    string
	Model     string
}

// Key returns the adapter-registry key for the binding: "transport" or
// "transport:family" when a family is present.
func (b Binding) Key() string {
	if b.Family == "" {
		return b.Transport
	}
	return b.Transport + ":" + b.Family
}

// ParseBinding splits a model-binding string. It rejects empty segments so a
// typo fails at config time, not at first completion.
func ParseBinding(s string) (Binding, error) {
	transport, rest, ok := strings.Cut(s, ":")
	if !ok || transport == "" || rest == "" {
		return Binding{}, fmt.Errorf("provider: model binding %q must be <transport>:[<family>/]<model>", s)
	}
	family, model, hasFamily := strings.Cut(rest, "/")
	if !hasFamily {
		return Binding{Transport: transport, Model: rest}, nil
	}
	if family == "" || model == "" {
		return Binding{}, fmt.Errorf("provider: model binding %q has an empty family or model segment", s)
	}
	return Binding{Transport: transport, Family: family, Model: model}, nil
}

// Config is the per-node backend configuration adapters may need beyond the
// binding itself. Vertex adapters require Project; Location defaults are
// adapter-specific.
type Config struct {
	Project  string
	Location string
}

// Factory builds a Provider for one registry key.
type Factory func(cfg Config) (Provider, error)

// registry maps Binding.Key() → Factory. Stage 0 registers only
// vertex:anthropic (see anthropic_vertex.go init).
var registry = map[string]Factory{}

// RegisterFactory installs a factory under key. Re-registering a key panics —
// adapter wiring is static, a collision is a programming error.
func RegisterFactory(key string, f Factory) {
	if _, ok := registry[key]; ok {
		panic(fmt.Sprintf("provider: factory %q already registered", key))
	}
	registry[key] = f
}

// For resolves a model-binding string into a live Provider plus the
// adapter-local model id.
func For(binding string, cfg Config) (Provider, string, error) {
	b, err := ParseBinding(binding)
	if err != nil {
		return nil, "", err
	}
	f, ok := registry[b.Key()]
	if !ok {
		return nil, "", fmt.Errorf("provider: no adapter registered for %q (binding %q); stage 0 ships vertex:anthropic only", b.Key(), binding)
	}
	p, err := f(cfg)
	if err != nil {
		return nil, "", err
	}
	return p, b.Model, nil
}

// WireToolName transcodes a dotted tool name into the wire-safe underscore
// form ("fs.read" → "fs_read"). See ToolSchema for the bijectivity caveat.
func WireToolName(dotted string) string { return strings.ReplaceAll(dotted, ".", "_") }

// DottedToolName is the inverse of WireToolName.
func DottedToolName(wire string) string { return strings.ReplaceAll(wire, "_", ".") }
