package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"google.golang.org/genai"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
)

// llm_gemini is the "filler" LLM node: a pure translation between the event
// log and a Vertex AI Gemini completion. It holds NO agent logic — no rules,
// no tool routing, no termination decision. On every trigger it folds the
// request's event log into a transcript, calls the model once, and emits the
// raw completion verbatim as a single llm.completed event. Deciding what the
// completion MEANS is the decoder's job (see llm_decode.go).
//
// The node is deliberately not idempotent: the same trigger event with the
// same log can produce a different completion (sampling, model drift). This
// is a known violation of the pure-subscriber ideal and one of the failure
// modes the react example exists to surface.
type LLMGeminiConfig struct {
	Project  string           `json:"project"`
	Location string           `json:"location"` // "global" or a region
	Model    string           `json:"model"`
	System   string           `json:"system"` // optional extra instructions
	Emit     string           `json:"emit"`   // emitted event type, default llm.completed
	Tools    []llmGeminiTools `json:"tools"`  // menu advertised to the model
}

type llmGeminiTools struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// TypeLLMCompleted is the default raw-completion event type. It is
// non-terminal: a completion is a demand for the decoder to react, not a
// closed branch.
const TypeLLMCompleted = "llm.completed"

// TypeLLMFailed is the non-terminal error event emitted when the backend call
// fails. Per the domain model, errors are events, not Go errors — returning a
// Go error from React would abort the whole drain, so a backend failure
// becomes a `<node>.failed`-style event the topology can react to instead.
const TypeLLMFailed = "llm.failed"

// GeminiBackend is the swappable transport behind llm_gemini. The production
// implementation wraps the official google.golang.org/genai client and picks
// up Application Default Credentials automatically — no tokens are ever
// threaded through reflex. Unit tests inject a fake via SetGeminiBackend so
// the handler is exercised without touching the network using a
// dependency-injected backend interface.
type GeminiBackend interface {
	// Generate calls the model ONCE with the rendered system prompt and the
	// folded transcript, returning the raw completion text.
	Generate(ctx context.Context, cfg LLMGeminiConfig, system, transcript string) (string, error)
}

// defaultGeminiBackend is overridable for runtime injection (e.g. tests).
var (
	geminiBackendMu sync.Mutex
	geminiBackend   GeminiBackend = &vertexGeminiBackend{}
)

// SetGeminiBackend swaps the backend used by every llm_gemini handler and
// returns the previous one so callers can restore it. Tests use this to
// inject a mock without rewriting the YAML factory.
func SetGeminiBackend(b GeminiBackend) GeminiBackend {
	geminiBackendMu.Lock()
	defer geminiBackendMu.Unlock()
	prev := geminiBackend
	if b == nil {
		b = &vertexGeminiBackend{}
	}
	geminiBackend = b
	return prev
}

func currentGeminiBackend() GeminiBackend {
	geminiBackendMu.Lock()
	defer geminiBackendMu.Unlock()
	return geminiBackend
}

// vertexGeminiBackend is the real transport. It lazily constructs one genai
// client on first use and reuses it for the lifetime of the process — the
// client is concurrency-safe and ADC discovery is the expensive part, so
// caching it keeps repeated turns cheap.
type vertexGeminiBackend struct {
	mu     sync.Mutex
	client *genai.Client
	// key remembers which {project, location} the cached client was built
	// for; if a handler asks for a different pair we rebuild.
	key string
}

func (v *vertexGeminiBackend) Generate(ctx context.Context, cfg LLMGeminiConfig, system, transcript string) (string, error) {
	client, err := v.clientFor(ctx, cfg)
	if err != nil {
		return "", err
	}

	// One shot, deterministic (temperature 0), JSON-mode so the model is
	// nudged toward the single-object contract the decoder expects.
	resp, err := client.Models.GenerateContent(ctx, cfg.Model, genai.Text(transcript), &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText(system, genai.RoleUser),
		Temperature:       genai.Ptr(float32(0)),
		ResponseMIMEType:  "application/json",
	})
	if err != nil {
		return "", fmt.Errorf("vertex generateContent: %w", err)
	}
	return resp.Text(), nil
}

// clientFor returns a cached genai client for cfg's {project, location},
// rebuilding it if the previous client was built for a different pair. ADC is
// picked up automatically by the SDK; reflex never handles a bearer token.
func (v *vertexGeminiBackend) clientFor(ctx context.Context, cfg LLMGeminiConfig) (*genai.Client, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	key := cfg.Project + "|" + cfg.Location
	if v.client != nil && v.key == key {
		return v.client, nil
	}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		Backend:  genai.BackendVertexAI,
		Project:  cfg.Project,
		Location: cfg.Location,
	})
	if err != nil {
		return nil, fmt.Errorf("genai.NewClient: %w", err)
	}
	v.client = client
	v.key = key
	return client, nil
}

func newLLMGemini(cfg config.HandlerConfig) (bus.Subscriber, error) {
	var gc LLMGeminiConfig
	if err := decodeConfig(cfg.Config, &gc); err != nil {
		return nil, fmt.Errorf("llm_gemini %q: %w", cfg.Name, err)
	}
	if gc.Project == "" || gc.Model == "" {
		return nil, fmt.Errorf("llm_gemini %q: project and model are required", cfg.Name)
	}
	if gc.Location == "" {
		gc.Location = "global"
	}
	if gc.Emit == "" {
		gc.Emit = TypeLLMCompleted
	}

	on := cfg.On
	name := cfg.Name
	return &genericSub{
		baseSub: baseSub{name: name},
		on:      on,
		run: func(ctx context.Context, ev event.Event, log []event.Event) ([]event.Event, error) {
			transcript := renderTranscript(log, ev.RequestID)
			text, err := currentGeminiBackend().Generate(ctx, gc, systemPrompt(gc), transcript)
			if err != nil {
				// Errors are events, not control flow. Returning a Go error
				// here would abort the entire drain; instead we emit a
				// non-terminal llm.failed so the topology can react (retry,
				// fallback) or visibly orphan.
				failPayload, mErr := json.Marshal(map[string]string{"error": err.Error()})
				if mErr != nil {
					return nil, mErr
				}
				return []event.Event{{
					Type:    TypeLLMFailed,
					Payload: failPayload,
				}}, nil
			}
			payload, err := json.Marshal(map[string]string{"text": text})
			if err != nil {
				return nil, err
			}
			return []event.Event{{
				Type:    gc.Emit,
				Payload: payload,
			}}, nil
		},
	}, nil
}

// renderTranscript folds the request's event log into a generic textual
// scratchpad: one line per event, `type: payload`. It is deliberately
// domain-blind — no special-casing of tool calls or assistant messages. The
// only filtering is bus meta-events (routing noise, not conversation) and
// events belonging to other requests.
func renderTranscript(log []event.Event, requestID string) string {
	var b strings.Builder
	for _, e := range log {
		if e.RequestID != requestID {
			continue
		}
		if isBusMeta(e.Type) {
			continue
		}
		b.WriteString(e.Type)
		if len(e.Payload) > 0 && string(e.Payload) != "null" {
			b.WriteString(": ")
			b.Write(e.Payload)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func isBusMeta(typ string) bool {
	switch typ {
	case bus.EventDispatchedType, bus.DrainQuiescedType, bus.HandlerFailedType, bus.LoopExhaustedType:
		return true
	}
	return false
}

// systemPrompt assembles the fixed instruction block. The contract with the
// decoder: respond with exactly one JSON object, either a tool action or a
// final answer. The tool menu comes from YAML config — deriving it from the
// subscription table (Introspect) is the designed end state, not done yet.
func systemPrompt(gc LLMGeminiConfig) string {
	var b strings.Builder
	b.WriteString("You are the reasoning node inside an event-driven agent. ")
	b.WriteString("You receive the append-only event log of one request as the user message. ")
	b.WriteString("Decide the single next step.\n\n")
	b.WriteString("Respond with ONLY one JSON object, no prose, no markdown fences:\n")
	b.WriteString(`  {"action":"tool","tool":"<name>","args":"<args string>"}` + "\n")
	b.WriteString(`  {"action":"final","text":"<answer to the user>"}` + "\n\n")
	if len(gc.Tools) > 0 {
		b.WriteString("Available tools:\n")
		for _, t := range gc.Tools {
			b.WriteString("  - " + t.Name + ": " + t.Description + "\n")
		}
	}
	b.WriteString("\nUse a tool when the request needs it; answer with action=final when the log already contains everything needed.")
	if gc.System != "" {
		b.WriteString("\n\n" + gc.System)
	}
	return b.String()
}
