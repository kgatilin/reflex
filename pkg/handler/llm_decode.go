package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/projection"
)

// llm_decode is pure protocol translation. It consumes the raw completion the
// llm_gemini node emits (llm.completed{text}) and turns the model's
// single-JSON-object contract into the subject-typed events the rest of the
// graph routes on. It holds NO model logic — it never calls a model, never
// decides anything beyond "what does this JSON say to do next".
//
// The decoder is the seam where the domain-blind reasoning node meets the
// typed event universe: a tool action becomes a tool.<name>.call; a final
// answer becomes assistant.message + RequestHandled. Keeping this split means
// the LLM node can be swapped wholesale without touching routing, and the
// routing can be reshaped without re-prompting the model.
type llmDecodeConfig struct {
	// On is read from cfg.On (the trigger event type), not from config.
	// Tools is the allowlist of tool names the decoder is willing to route
	// to. Each name produces a declared tool.<name>.call emission so the
	// static graph sees the edge.
	Tools []string `json:"tools"`
	// Strict, when true, rejects a tool action whose name is not in Tools
	// (emitting llm.emission_rejected) instead of forwarding it. Default
	// false: an unknown tool is forwarded verbatim as tool.<unknown>.call,
	// which the static graph will then flag as a type-level gap (no
	// consumer for tool.<unknown>.call).
	Strict bool `json:"strict"`
}

// Decoder output event types. assistant.message and RequestHandled are the
// terminal pair that closes a request on a final answer; the rest are
// non-terminal (they demand a downstream reaction or visibly orphan).
const (
	TypeAssistantMessage    = "assistant.message"
	TypeLLMDecodeFailed     = "llm.decode_failed"
	TypeLLMEmissionRejected = "llm.emission_rejected"
)

// llmAction is the single JSON object the model is contracted to emit.
type llmAction struct {
	Action string `json:"action"` // "tool" | "final"
	Tool   string `json:"tool"`   // set when Action == "tool"
	Args   string `json:"args"`   // set when Action == "tool"
	Text   string `json:"text"`   // set when Action == "final"
}

func newLLMDecode(cfg config.HandlerConfig) (bus.Subscriber, error) {
	var dc llmDecodeConfig
	if err := decodeConfig(cfg.Config, &dc); err != nil {
		return nil, fmt.Errorf("llm_decode %q: %w", cfg.Name, err)
	}

	// Build a set of allowed tool names for the strict check.
	allowed := make(map[string]bool, len(dc.Tools))
	for _, t := range dc.Tools {
		allowed[t] = true
	}
	strict := dc.Strict

	on := cfg.On
	name := cfg.Name
	return &genericSub{
		baseSub: baseSub{name: name},
		on:      on,
		run: func(_ context.Context, ev event.Event, _ []event.Event) ([]event.Event, error) {
			var p struct {
				Text string `json:"text"`
			}
			if err := ev.PayloadAs(&p); err != nil {
				return nil, fmt.Errorf("llm_decode %q: decode payload: %w", name, err)
			}

			raw := extractJSONObject(p.Text)
			var act llmAction
			if raw == "" || json.Unmarshal([]byte(raw), &act) != nil {
				// Unparseable: emit a NON-terminal decode failure so the
				// topology can react (e.g. re-prompt) or visibly orphan.
				failPayload, err := json.Marshal(map[string]string{
					"error": "could not parse a JSON action object",
					"raw":   p.Text,
				})
				if err != nil {
					return nil, err
				}
				return []event.Event{{
					Type:    TypeLLMDecodeFailed,
					Payload: failPayload,
				}}, nil
			}

			switch act.Action {
			case "tool":
				// Strict mode rejects tools outside the allowlist instead of
				// minting a tool.<unknown>.call the graph can't route.
				if strict && !allowed[act.Tool] {
					rejPayload, err := json.Marshal(map[string]string{
						"tool":   act.Tool,
						"reason": "tool not in decoder allowlist",
					})
					if err != nil {
						return nil, err
					}
					return []event.Event{{
						Type:    TypeLLMEmissionRejected,
						Payload: rejPayload,
					}}, nil
				}
				// Emit tool.<tool>.call{args}. Routing is in the TYPE; the
				// matching tool_node subscribes to exactly this subject.
				callPayload, err := json.Marshal(map[string]string{"args": act.Args})
				if err != nil {
					return nil, err
				}
				return []event.Event{{
					Type:    "tool." + act.Tool + ".call",
					Payload: callPayload,
				}}, nil

			case "final":
				// A final answer closes the request: the assistant message
				// and RequestHandled are both terminal leaves of the cone.
				msgPayload, err := json.Marshal(map[string]string{"text": act.Text})
				if err != nil {
					return nil, err
				}
				return []event.Event{
					{Type: TypeAssistantMessage, Payload: msgPayload, Terminal: true},
					{Type: projection.TypeRequestHandled, Terminal: true},
				}, nil

			default:
				// A well-formed JSON object with an unknown action verb is
				// still a decode failure from the contract's perspective.
				failPayload, err := json.Marshal(map[string]string{
					"error": fmt.Sprintf("unknown action %q", act.Action),
					"raw":   p.Text,
				})
				if err != nil {
					return nil, err
				}
				return []event.Event{{
					Type:    TypeLLMDecodeFailed,
					Payload: failPayload,
				}}, nil
			}
		},
	}, nil
}

// extractJSONObject pulls the first balanced-looking JSON object out of s. It
// strips an optional ```json … ``` (or bare ```) fence and then takes the
// substring from the first '{' to the last '}'. This is deliberately lenient:
// models sometimes wrap their answer in a fence or add a trailing newline,
// and the contract only promises "one JSON object", not "the entire response
// is exactly that object".
func extractJSONObject(s string) string {
	s = strings.TrimSpace(s)
	// Strip a leading fence line (```json or ```), if any.
	if strings.HasPrefix(s, "```") {
		if nl := strings.IndexByte(s, '\n'); nl != -1 {
			s = s[nl+1:]
		}
		// Drop a trailing fence.
		if idx := strings.LastIndex(s, "```"); idx != -1 {
			s = s[:idx]
		}
		s = strings.TrimSpace(s)
	}
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start == -1 || end == -1 || end < start {
		return ""
	}
	return s[start : end+1]
}
