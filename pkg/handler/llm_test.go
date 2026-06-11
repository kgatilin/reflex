package handler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/projection"
	"github.com/kgatilin/reflex/pkg/provider"
)

// fakeProvider returns a queued response (or error) per Complete call and
// records the requests it received.
type fakeProvider struct {
	resp provider.Response
	err  error
	got  []provider.Request
}

func (f *fakeProvider) Complete(_ context.Context, req provider.Request) (provider.Response, error) {
	f.got = append(f.got, req)
	if f.err != nil {
		return provider.Response{}, f.err
	}
	return f.resp, nil
}

func withFakeProvider(t *testing.T, f *fakeProvider) {
	t.Helper()
	prev := SetProviderResolver(func(binding string, _ provider.Config) (provider.Provider, string, error) {
		b, err := provider.ParseBinding(binding)
		if err != nil {
			return nil, "", err
		}
		return f, b.Model, nil
	})
	t.Cleanup(func() { SetProviderResolver(prev) })
}

func llmHandlerForTest(t *testing.T) *genericSub {
	t.Helper()
	sub, err := newLLM(config.HandlerConfig{
		Name: "brain",
		Type: "llm",
		On:   "llm.turn",
		Config: map[string]any{
			"model":   "vertex:anthropic/claude-opus-4-8",
			"project": "proj",
			"tools": []any{
				map[string]any{
					"name":        "fs.read",
					"description": "read a file window",
					"input_schema": map[string]any{
						"type":       "object",
						"properties": map[string]any{"path": map[string]any{"type": "string"}},
						"required":   []any{"path"},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("newLLM: %v", err)
	}
	return sub.(*genericSub)
}

func reactLLM(t *testing.T, sub *genericSub) []event.Event {
	t.Helper()
	trigger := event.Event{Type: "llm.turn", RequestID: "r1"}
	log := []event.Event{
		{Type: "RequestReceived", RequestID: "r1", Payload: json.RawMessage(`{"payload":"fix Parse"}`)},
		trigger,
	}
	out, err := sub.React(context.Background(), trigger, log)
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	return out
}

func byType(events []event.Event) map[string][]event.Event {
	m := map[string][]event.Event{}
	for _, e := range events {
		m[e.Type] = append(m[e.Type], e)
	}
	return m
}

func TestLLMToolCallEmission(t *testing.T) {
	fake := &fakeProvider{resp: provider.Response{
		ToolCalls:  []provider.ToolCall{{ID: "tu1", Name: "fs.read", Input: json.RawMessage(`{"path":"pkg/foo/bar.go"}`)}},
		StopReason: "tool_use",
		Usage:      provider.Usage{InputTokens: 100, OutputTokens: 20},
	}}
	withFakeProvider(t, fake)
	out := reactLLM(t, llmHandlerForTest(t))
	m := byType(out)

	calls := m["tool.fs.read.call"]
	if len(calls) != 1 {
		t.Fatalf("tool.fs.read.call events = %d; all: %+v", len(calls), out)
	}
	if calls[0].Terminal {
		t.Error("tool call must be non-terminal (demands a plugin reaction)")
	}
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(calls[0].Payload, &in); err != nil || in.Path != "pkg/foo/bar.go" {
		t.Errorf("call payload = %s", calls[0].Payload)
	}

	usage := m[TypeLLMUsage]
	if len(usage) != 1 || !usage[0].Terminal {
		t.Fatalf("llm.usage = %+v", usage)
	}
	var u struct {
		Model        string `json:"model"`
		InputTokens  int64  `json:"input_tokens"`
		OutputTokens int64  `json:"output_tokens"`
		StopReason   string `json:"stop_reason"`
	}
	if err := json.Unmarshal(usage[0].Payload, &u); err != nil {
		t.Fatalf("usage payload: %v", err)
	}
	if u.Model != "vertex:anthropic/claude-opus-4-8" || u.InputTokens != 100 || u.OutputTokens != 20 || u.StopReason != "tool_use" {
		t.Errorf("usage = %+v", u)
	}

	// The provider saw the tool menu and the folded transcript.
	req := fake.got[0]
	if len(req.Tools) != 1 || req.Tools[0].Name != "fs.read" {
		t.Errorf("tools sent = %+v", req.Tools)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Fatalf("messages = %+v", req.Messages)
	}
	if req.Model != "claude-opus-4-8" {
		t.Errorf("adapter-local model = %q", req.Model)
	}
}

func TestLLMFinalAnswer(t *testing.T) {
	fake := &fakeProvider{resp: provider.Response{
		Text:       "Parse now returns an error on empty input.",
		StopReason: "end_turn",
		Usage:      provider.Usage{InputTokens: 50, OutputTokens: 10},
	}}
	withFakeProvider(t, fake)
	out := reactLLM(t, llmHandlerForTest(t))
	m := byType(out)

	msgs := m[TypeAssistantMessage]
	if len(msgs) != 1 || !msgs[0].Terminal {
		t.Fatalf("assistant.message = %+v", msgs)
	}
	handled := m[projection.TypeRequestHandled]
	if len(handled) != 1 || !handled[0].Terminal {
		t.Fatalf("RequestHandled = %+v", handled)
	}
	if len(m[TypeLLMUsage]) != 1 {
		t.Fatalf("llm.usage missing: %+v", out)
	}
}

func TestLLMParallelCallsDropped(t *testing.T) {
	fake := &fakeProvider{resp: provider.Response{
		ToolCalls: []provider.ToolCall{
			{Name: "fs.read", Input: json.RawMessage(`{"path":"a.go"}`)},
			{Name: "fs.read", Input: json.RawMessage(`{"path":"b.go"}`)},
			{Name: "go.vet", Input: json.RawMessage(`{}`)},
		},
		StopReason: "tool_use",
	}}
	withFakeProvider(t, fake)
	out := reactLLM(t, llmHandlerForTest(t))
	m := byType(out)

	if len(m["tool.fs.read.call"]) != 1 {
		t.Fatalf("want exactly the first call forwarded, got %+v", out)
	}
	if len(m["tool.go.vet.call"]) != 0 {
		t.Fatal("surplus call must not be forwarded at stage 0")
	}
	dropped := m[TypeLLMCallsDropped]
	if len(dropped) != 1 || !dropped[0].Terminal {
		t.Fatalf("llm.calls_dropped = %+v", dropped)
	}
	var d struct {
		Dropped []string `json:"dropped"`
	}
	if err := json.Unmarshal(dropped[0].Payload, &d); err != nil || len(d.Dropped) != 2 {
		t.Errorf("dropped payload = %s", dropped[0].Payload)
	}
}

func TestLLMProviderErrorBecomesFailedEvent(t *testing.T) {
	fake := &fakeProvider{err: errors.New("auth boom")}
	withFakeProvider(t, fake)
	out := reactLLM(t, llmHandlerForTest(t))
	m := byType(out)
	failed := m[TypeLLMFailed]
	if len(failed) != 1 || failed[0].Terminal {
		t.Fatalf("llm.failed = %+v (must be exactly one, non-terminal)", out)
	}
	if len(m[TypeLLMUsage]) != 0 {
		t.Error("no usage event on a failed call")
	}
}

func TestLLMEmptyCompletionFails(t *testing.T) {
	fake := &fakeProvider{resp: provider.Response{StopReason: "end_turn"}}
	withFakeProvider(t, fake)
	out := reactLLM(t, llmHandlerForTest(t))
	m := byType(out)
	if len(m[TypeLLMFailed]) != 1 {
		t.Fatalf("want llm.failed for empty completion, got %+v", out)
	}
	// Usage is still reported — the tokens were spent.
	if len(m[TypeLLMUsage]) != 1 {
		t.Fatalf("usage must be reported even for empty completions: %+v", out)
	}
}

func TestLLMConfigValidation(t *testing.T) {
	if _, err := newLLM(config.HandlerConfig{Name: "b", Config: map[string]any{}}); err == nil {
		t.Error("want error when model binding is missing")
	}
	if _, err := newLLM(config.HandlerConfig{Name: "b", Config: map[string]any{"model": "no-transport"}}); err == nil {
		t.Error("want error for malformed binding")
	}
	if _, err := newLLM(config.HandlerConfig{Name: "b", Config: map[string]any{
		"model": "vertex:anthropic/claude-x",
		"tools": []any{map[string]any{"description": "nameless"}},
	}}); err == nil {
		t.Error("want error for a nameless tool")
	}
}

func TestLLMSpecResolver(t *testing.T) {
	cfg := config.HandlerConfig{
		Name: "brain",
		Type: "llm",
		On:   "llm.turn",
		Config: map[string]any{
			"model": "vertex:anthropic/claude-x",
			"tools": []any{
				map[string]any{"name": "fs.read"},
				map[string]any{"name": "go.build"},
			},
		},
	}
	spec := llmSpecResolver(cfg, llmSpec())
	types := map[string]EmittedSpec{}
	for _, e := range spec.Emits {
		types[e.Type] = e
	}
	for _, want := range []string{"tool.fs.read.call", "tool.go.build.call", TypeAssistantMessage, projection.TypeRequestHandled, TypeLLMUsage, TypeLLMFailed, TypeLLMCallsDropped} {
		if _, ok := types[want]; !ok {
			t.Errorf("spec missing emission %q; got %+v", want, spec.Emits)
		}
	}
	if !types[TypeLLMUsage].Terminal {
		t.Error("llm.usage must be terminal in the spec")
	}
}
