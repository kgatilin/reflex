package provider

import (
	"encoding/json"
	"testing"
)

var _ Provider = (*maasVertex)(nil)

func TestForMaasRequiresProject(t *testing.T) {
	for _, key := range []string{"vertex:meta/llama-3.3-70b-instruct-maas", "vertex:deepseek-ai/deepseek-r1-0528-maas", "vertex:qwen/qwen3-235b-a22b-instruct-maas"} {
		if _, _, err := For(key, Config{}); err == nil {
			t.Errorf("want error when project is missing for %q", key)
		}
		p, model, err := For(key, Config{Project: "my-project"})
		if err != nil {
			t.Fatalf("For: %v", err)
		}
		mv, ok := p.(*maasVertex)
		if !ok {
			t.Fatalf("provider type %T, want *maasVertex", p)
		}
		if mv.cfg.Location != "us-central1" {
			t.Errorf("location default = %q, want us-central1", mv.cfg.Location)
		}
		var expectedModel string
		switch mv.family {
		case "meta":
			expectedModel = "llama-3.3-70b-instruct-maas"
		case "deepseek-ai":
			expectedModel = "deepseek-r1-0528-maas"
		case "qwen":
			expectedModel = "qwen3-235b-a22b-instruct-maas"
		}
		if model != expectedModel {
			t.Errorf("got model = %q, want %q", model, expectedModel)
		}
	}
}

func TestMaasBuildRequest(t *testing.T) {
	mv := &maasVertex{
		cfg:    Config{Project: "my-project", Location: "us-central1"},
		family: "deepseek-ai",
	}

	req := Request{
		Model:  "deepseek-r1-0528-maas",
		System: "You are a helpful assistant.",
		Messages: []Message{
			{Role: "user", Text: "Hello!"},
			{Role: "assistant", Text: "Hi! How can I help?"},
		},
		Tools: []ToolSchema{
			{
				Name:        "fs.read",
				Description: "Read file contents",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
			},
		},
		MaxTokens: 500,
	}

	maasReq, nameMap, err := mv.buildRequest(req)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}

	if maasReq.Model != "deepseek-ai/deepseek-r1-0528-maas" {
		t.Errorf("model = %q, want deepseek-ai/deepseek-r1-0528-maas", maasReq.Model)
	}

	if maasReq.MaxTokens != 500 {
		t.Errorf("max_tokens = %d, want 500", maasReq.MaxTokens)
	}

	if len(maasReq.Messages) != 3 {
		t.Fatalf("len(messages) = %d, want 3", len(maasReq.Messages))
	}

	if maasReq.Messages[0].Role != "system" || maasReq.Messages[0].Content != "You are a helpful assistant." {
		t.Errorf("messages[0] = %+v", maasReq.Messages[0])
	}

	if maasReq.Messages[1].Role != "user" || maasReq.Messages[1].Content != "Hello!" {
		t.Errorf("messages[1] = %+v", maasReq.Messages[1])
	}

	if maasReq.Messages[2].Role != "assistant" || maasReq.Messages[2].Content != "Hi! How can I help?" {
		t.Errorf("messages[2] = %+v", maasReq.Messages[2])
	}

	if len(maasReq.Tools) != 1 {
		t.Fatalf("len(tools) = %d, want 1", len(maasReq.Tools))
	}

	tool := maasReq.Tools[0]
	if tool.Type != "function" {
		t.Errorf("tool type = %q, want function", tool.Type)
	}
	if tool.Function.Name != "fs_read" {
		t.Errorf("tool function name = %q, want fs_read", tool.Function.Name)
	}
	if tool.Function.Description != "Read file contents" {
		t.Errorf("tool description = %q, want Read file contents", tool.Function.Description)
	}
	if string(tool.Function.Parameters) != `{"type":"object","properties":{"path":{"type":"string"}}}` {
		t.Errorf("tool parameters = %s", string(tool.Function.Parameters))
	}

	if nameMap["fs_read"] != "fs.read" {
		t.Errorf("nameMap[fs_read] = %q, want fs.read", nameMap["fs_read"])
	}
}

func TestDecodeMaasResponse(t *testing.T) {
	nameMap := map[string]string{
		"fs_read": "fs.read",
	}

	var rawResponse maasResponse
	jsonBytes := []byte(`{
		"choices": [
			{
				"message": {
					"role": "assistant",
					"content": "I will read the file now.",
					"tool_calls": [
						{
							"id": "call_123",
							"type": "function",
							"function": {
								"name": "fs_read",
								"arguments": "{\"path\": \"main.go\"}"
							}
						}
					]
				},
				"finish_reason": "tool_calls"
			}
		],
		"usage": {
			"prompt_tokens": 120,
			"completion_tokens": 45
		}
	}`)

	if err := json.Unmarshal(jsonBytes, &rawResponse); err != nil {
		t.Fatalf("failed to unmarshal test JSON: %v", err)
	}

	resp := decodeMaasResponse(rawResponse, nameMap)

	if resp.Text != "I will read the file now." {
		t.Errorf("Text = %q, want 'I will read the file now.'", resp.Text)
	}

	if resp.StopReason != "tool_calls" {
		t.Errorf("StopReason = %q, want 'tool_calls'", resp.StopReason)
	}

	if len(resp.ToolCalls) != 1 {
		t.Fatalf("len(ToolCalls) = %d, want 1", len(resp.ToolCalls))
	}

	tc := resp.ToolCalls[0]
	if tc.ID != "call_123" {
		t.Errorf("tc.ID = %q, want 'call_123'", tc.ID)
	}
	if tc.Name != "fs.read" {
		t.Errorf("tc.Name = %q, want 'fs.read'", tc.Name)
	}
	if string(tc.Input) != `{"path": "main.go"}` {
		t.Errorf("tc.Input = %q, want '{\"path\": \"main.go\"}'", string(tc.Input))
	}

	if resp.Usage.InputTokens != 120 {
		t.Errorf("Usage.InputTokens = %d, want 120", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 45 {
		t.Errorf("Usage.OutputTokens = %d, want 45", resp.Usage.OutputTokens)
	}
}
