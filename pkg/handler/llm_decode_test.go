package handler

import (
	"context"
	"testing"

	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/projection"
)

// decodeOnce runs the decoder over one llm.completed{text} payload.
func decodeOnce(t *testing.T, cfg map[string]any, text string) []event.Event {
	t.Helper()
	sub, err := newLLMDecode(config.HandlerConfig{
		Name: "decode", Type: "llm_decode", On: TypeLLMCompleted, Config: cfg,
	})
	if err != nil {
		t.Fatalf("newLLMDecode: %v", err)
	}
	ev := event.Event{
		Type:    TypeLLMCompleted,
		Payload: jsonRaw(map[string]string{"text": text}),
	}
	out, err := sub.React(context.Background(), ev, nil)
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	return out
}

func TestDecodeToolAction(t *testing.T) {
	out := decodeOnce(t, map[string]any{"tools": []any{"calc"}},
		`{"action":"tool","tool":"calc","args":"23*7"}`)
	if len(out) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(out), out)
	}
	if out[0].Type != "tool.calc.call" {
		t.Fatalf("type = %s, want tool.calc.call", out[0].Type)
	}
	if out[0].Terminal {
		t.Fatal("tool.calc.call must be non-terminal")
	}
	var p struct {
		Args string `json:"args"`
	}
	_ = out[0].PayloadAs(&p)
	if p.Args != "23*7" {
		t.Fatalf("args = %q", p.Args)
	}
}

func TestDecodeFinalAction(t *testing.T) {
	out := decodeOnce(t, map[string]any{"tools": []any{"calc"}},
		`{"action":"final","text":"172"}`)
	if len(out) != 2 {
		t.Fatalf("expected 2 events (assistant.message + RequestHandled), got %d: %+v", len(out), out)
	}
	if out[0].Type != TypeAssistantMessage || !out[0].Terminal {
		t.Fatalf("event[0] = %+v, want terminal assistant.message", out[0])
	}
	if out[1].Type != projection.TypeRequestHandled || !out[1].Terminal {
		t.Fatalf("event[1] = %+v, want terminal RequestHandled", out[1])
	}
	var p struct {
		Text string `json:"text"`
	}
	_ = out[0].PayloadAs(&p)
	if p.Text != "172" {
		t.Fatalf("text = %q", p.Text)
	}
}

func TestDecodeFencedJSON(t *testing.T) {
	// A model wrapping its answer in a ```json fence must still decode.
	fenced := "```json\n{\"action\":\"final\",\"text\":\"ok\"}\n```"
	out := decodeOnce(t, map[string]any{"tools": []any{"calc"}}, fenced)
	if len(out) != 2 || out[0].Type != TypeAssistantMessage {
		t.Fatalf("fenced JSON not decoded: %+v", out)
	}
}

func TestDecodeGarbageEmitsDecodeFailedNonTerminal(t *testing.T) {
	out := decodeOnce(t, map[string]any{"tools": []any{"calc"}},
		"I think the answer is probably 42 but I'm not sure")
	if len(out) != 1 || out[0].Type != TypeLLMDecodeFailed {
		t.Fatalf("expected one llm.decode_failed, got %+v", out)
	}
	if out[0].Terminal {
		t.Fatal("llm.decode_failed must be NON-terminal")
	}
	var p struct {
		Raw string `json:"raw"`
	}
	_ = out[0].PayloadAs(&p)
	if p.Raw == "" {
		t.Fatal("decode_failed must carry the raw text")
	}
}

func TestDecodeUnknownToolStrictRejects(t *testing.T) {
	out := decodeOnce(t, map[string]any{"tools": []any{"calc"}, "strict": true},
		`{"action":"tool","tool":"frobnicate","args":"x"}`)
	if len(out) != 1 || out[0].Type != TypeLLMEmissionRejected {
		t.Fatalf("strict mode should reject unknown tool, got %+v", out)
	}
	if out[0].Terminal {
		t.Fatal("llm.emission_rejected must be non-terminal")
	}
	var p struct {
		Tool string `json:"tool"`
	}
	_ = out[0].PayloadAs(&p)
	if p.Tool != "frobnicate" {
		t.Fatalf("rejected tool = %q", p.Tool)
	}
}

func TestDecodeUnknownToolNonStrictForwards(t *testing.T) {
	// Default (strict=false): unknown tool is forwarded verbatim so the
	// static graph flags it as a type-level gap downstream.
	out := decodeOnce(t, map[string]any{"tools": []any{"calc"}},
		`{"action":"tool","tool":"frobnicate","args":"x"}`)
	if len(out) != 1 || out[0].Type != "tool.frobnicate.call" {
		t.Fatalf("non-strict should forward, got %+v", out)
	}
}
