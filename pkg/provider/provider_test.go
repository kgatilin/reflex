package provider

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestParseBinding(t *testing.T) {
	cases := []struct {
		in      string
		want    Binding
		wantKey string
		wantErr bool
	}{
		{in: "vertex:anthropic/claude-opus-4-8", want: Binding{"vertex", "anthropic", "claude-opus-4-8"}, wantKey: "vertex:anthropic"},
		{in: "vertex:gemini-2.5-pro", want: Binding{"vertex", "", "gemini-2.5-pro"}, wantKey: "vertex"},
		{in: "vertex:deepseek/deepseek-r1", want: Binding{"vertex", "deepseek", "deepseek-r1"}, wantKey: "vertex:deepseek"},
		{in: "claude-opus-4-8", wantErr: true},
		{in: "vertex:", wantErr: true},
		{in: "vertex:anthropic/", wantErr: true},
		{in: ":anthropic/x", wantErr: true},
	}
	for _, c := range cases {
		got, err := ParseBinding(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseBinding(%q): want error, got %+v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseBinding(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseBinding(%q) = %+v, want %+v", c.in, got, c.want)
		}
		if got.Key() != c.wantKey {
			t.Errorf("ParseBinding(%q).Key() = %q, want %q", c.in, got.Key(), c.wantKey)
		}
	}
}

func TestWireToolNameRoundtrip(t *testing.T) {
	for _, name := range []string{"fs.read", "fs.edit", "go.build", "go.test", "go.vet", "fs.search"} {
		wire := WireToolName(name)
		if wire == name {
			t.Errorf("WireToolName(%q) did not transcode", name)
		}
		if back := DottedToolName(wire); back != name {
			t.Errorf("roundtrip %q → %q → %q", name, wire, back)
		}
	}
}

func TestForUnknownBinding(t *testing.T) {
	if _, _, err := For("vertex:llama/llama-4", Config{Project: "p"}); err == nil {
		t.Fatal("want error for unregistered adapter key")
	}
}

func TestForAnthropicRequiresProject(t *testing.T) {
	if _, _, err := For("vertex:anthropic/claude-opus-4-8", Config{}); err == nil {
		t.Fatal("want error when project is missing")
	}
	p, model, err := For("vertex:anthropic/claude-opus-4-8", Config{Project: "proj"})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if model != "claude-opus-4-8" {
		t.Errorf("model = %q", model)
	}
	av, ok := p.(*anthropicVertex)
	if !ok {
		t.Fatalf("provider type %T", p)
	}
	if av.cfg.Location != defaultAnthropicLocation {
		t.Errorf("location default = %q, want %q", av.cfg.Location, defaultAnthropicLocation)
	}
}

func TestAnthropicParams(t *testing.T) {
	req := Request{
		Model:  "claude-opus-4-8",
		System: "be precise",
		Messages: []Message{
			{Role: "user", Text: "transcript here"},
		},
		Tools: []ToolSchema{{
			Name:        "fs.read",
			Description: "read a file window",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}},
	}
	params, err := anthropicParams(req)
	if err != nil {
		t.Fatalf("anthropicParams: %v", err)
	}
	if params.MaxTokens != 8192 {
		t.Errorf("MaxTokens default = %d", params.MaxTokens)
	}
	if string(params.Model) != "claude-opus-4-8" {
		t.Errorf("Model = %q", params.Model)
	}
	if len(params.System) != 1 || params.System[0].Text != "be precise" {
		t.Errorf("System = %+v", params.System)
	}
	if len(params.Messages) != 1 {
		t.Fatalf("Messages = %d", len(params.Messages))
	}
	if len(params.Tools) != 1 {
		t.Fatalf("Tools = %d", len(params.Tools))
	}
	tool := params.Tools[0].OfTool
	if tool.Name != "fs_read" {
		t.Errorf("wire tool name = %q, want fs_read (dots transcoded)", tool.Name)
	}
	if got := tool.InputSchema.Required; len(got) != 1 || got[0] != "path" {
		t.Errorf("required = %v", got)
	}
	props, ok := tool.InputSchema.Properties.(map[string]any)
	if !ok || props["path"] == nil {
		t.Errorf("properties = %#v", tool.InputSchema.Properties)
	}
}

func TestAnthropicParamsBadSchema(t *testing.T) {
	_, err := anthropicParams(Request{
		Model: "m",
		Tools: []ToolSchema{{Name: "x", InputSchema: json.RawMessage(`"not an object"`)}},
	})
	if err == nil {
		t.Fatal("want error for non-object input schema")
	}
}

func TestDecodeAnthropicMessage(t *testing.T) {
	raw := `{
		"id": "msg_1",
		"content": [
			{"type": "text", "text": "reading the file. "},
			{"type": "tool_use", "id": "tu_1", "name": "fs_read", "input": {"path": "pkg/foo/bar.go"}}
		],
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 1200, "output_tokens": 80, "cache_read_input_tokens": 900, "cache_creation_input_tokens": 100}
	}`
	var msg anthropic.Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	resp := decodeAnthropicMessage(&msg)
	if resp.Text != "reading the file. " {
		t.Errorf("Text = %q", resp.Text)
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("StopReason = %q", resp.StopReason)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d", len(resp.ToolCalls))
	}
	call := resp.ToolCalls[0]
	if call.Name != "fs.read" {
		t.Errorf("dotted name = %q", call.Name)
	}
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil || in.Path != "pkg/foo/bar.go" {
		t.Errorf("input = %s (err %v)", call.Input, err)
	}
	wantUsage := Usage{InputTokens: 1200, OutputTokens: 80, CacheReadTokens: 900, CacheCreationTokens: 100}
	if resp.Usage != wantUsage {
		t.Errorf("Usage = %+v, want %+v", resp.Usage, wantUsage)
	}
}

// Compile-time: the adapter satisfies the interface.
var _ Provider = (*anthropicVertex)(nil)

// Guard against accidental context misuse in the signature.
var _ = context.Background
