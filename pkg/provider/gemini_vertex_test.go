package provider

import (
	"encoding/json"
	"testing"

	"google.golang.org/genai"
)

// Compile-time: geminiVertex satisfies the Provider interface.
var _ Provider = (*geminiVertex)(nil)

func TestForGeminiRequiresProject(t *testing.T) {
	if _, _, err := For("vertex:gemini-2.5-flash", Config{}); err == nil {
		t.Fatal("want error when project is missing")
	}
	p, model, err := For("vertex:gemini-2.5-flash", Config{Project: "my-project"})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if model != "gemini-2.5-flash" {
		t.Errorf("model = %q, want gemini-2.5-flash", model)
	}
	gv, ok := p.(*geminiVertex)
	if !ok {
		t.Fatalf("provider type %T, want *geminiVertex", p)
	}
	if gv.cfg.Location != defaultGeminiLocation {
		t.Errorf("location default = %q, want %q", gv.cfg.Location, defaultGeminiLocation)
	}
}

func TestGeminiParams(t *testing.T) {
	req := Request{
		Model:  "gemini-2.5-flash",
		System: "be concise",
		Messages: []Message{
			{Role: "user", Text: "hello"},
			{Role: "assistant", Text: "hi there"},
			{Role: "user", Text: "what is 2+2?"},
		},
		Tools: []ToolSchema{{
			Name:        "fs.read",
			Description: "read a file window",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}},
		MaxTokens: 1024,
	}
	contents, cfg, err := geminiParams(req)
	if err != nil {
		t.Fatalf("geminiParams: %v", err)
	}

	// MaxTokens forwarded correctly.
	if cfg.MaxOutputTokens != 1024 {
		t.Errorf("MaxOutputTokens = %d, want 1024", cfg.MaxOutputTokens)
	}

	// System instruction set.
	if cfg.SystemInstruction == nil {
		t.Fatal("SystemInstruction is nil")
	}
	if len(cfg.SystemInstruction.Parts) == 0 || cfg.SystemInstruction.Parts[0].Text != "be concise" {
		t.Errorf("SystemInstruction text = %+v", cfg.SystemInstruction.Parts)
	}

	// Three messages in order with correct roles.
	if len(contents) != 3 {
		t.Fatalf("contents = %d, want 3", len(contents))
	}
	if contents[0].Role != "user" {
		t.Errorf("contents[0].Role = %q, want user", contents[0].Role)
	}
	if contents[1].Role != "model" {
		t.Errorf("contents[1].Role = %q, want model (assistant maps to model)", contents[1].Role)
	}
	if contents[2].Role != "user" {
		t.Errorf("contents[2].Role = %q, want user", contents[2].Role)
	}

	// Tools: one tool with dots preserved in the name.
	if len(cfg.Tools) != 1 || len(cfg.Tools[0].FunctionDeclarations) != 1 {
		t.Fatalf("tools = %+v", cfg.Tools)
	}
	decl := cfg.Tools[0].FunctionDeclarations[0]
	if decl.Name != "fs.read" {
		t.Errorf("tool name = %q, want fs.read (dots are legal in Gemini function names)", decl.Name)
	}
	if decl.Description != "read a file window" {
		t.Errorf("tool description = %q", decl.Description)
	}
	if decl.ParametersJsonSchema == nil {
		t.Error("ParametersJsonSchema is nil")
	}
}

func TestGeminiParamsDefaultMaxTokens(t *testing.T) {
	_, cfg, err := geminiParams(Request{Model: "gemini-2.5-flash"})
	if err != nil {
		t.Fatalf("geminiParams: %v", err)
	}
	if cfg.MaxOutputTokens != 8192 {
		t.Errorf("default MaxOutputTokens = %d, want 8192", cfg.MaxOutputTokens)
	}
}

func TestGeminiParamsNoSystemNoTools(t *testing.T) {
	_, cfg, err := geminiParams(Request{Model: "gemini-2.5-flash", Messages: []Message{{Role: "user", Text: "hi"}}})
	if err != nil {
		t.Fatalf("geminiParams: %v", err)
	}
	if cfg.SystemInstruction != nil {
		t.Errorf("SystemInstruction should be nil when System is empty")
	}
	if len(cfg.Tools) != 0 {
		t.Errorf("Tools should be empty when no tools in request")
	}
}

func TestGeminiParamsBadSchema(t *testing.T) {
	_, _, err := geminiParams(Request{
		Model: "m",
		Tools: []ToolSchema{{Name: "x", InputSchema: json.RawMessage(`not json`)}},
	})
	if err == nil {
		t.Fatal("want error for invalid JSON schema")
	}
}

func TestGeminiParamsToolNamePreservesDots(t *testing.T) {
	tools := []ToolSchema{
		{Name: "go.build", Description: "build", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "fs.edit", Description: "edit", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	decls, err := geminiFunctionDeclarations(tools)
	if err != nil {
		t.Fatalf("geminiFunctionDeclarations: %v", err)
	}
	for i, d := range decls {
		if d.Name != tools[i].Name {
			t.Errorf("decl[%d].Name = %q, want %q (dots must be preserved)", i, d.Name, tools[i].Name)
		}
	}
}

func TestDecodeGeminiResponseTextOnly(t *testing.T) {
	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content: &genai.Content{
				Role: "model",
				Parts: []*genai.Part{
					{Text: "The answer is 42."},
				},
			},
			FinishReason: genai.FinishReasonStop,
		}},
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:        100,
			CandidatesTokenCount:    20,
			CachedContentTokenCount: 30,
		},
	}

	out := decodeGeminiResponse(resp)

	if out.Text != "The answer is 42." {
		t.Errorf("Text = %q", out.Text)
	}
	if out.StopReason != "stop" {
		t.Errorf("StopReason = %q, want stop", out.StopReason)
	}
	if len(out.ToolCalls) != 0 {
		t.Errorf("unexpected ToolCalls: %+v", out.ToolCalls)
	}
	want := Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 30}
	if out.Usage != want {
		t.Errorf("Usage = %+v, want %+v", out.Usage, want)
	}
}

func TestDecodeGeminiResponseFunctionCall(t *testing.T) {
	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content: &genai.Content{
				Role: "model",
				Parts: []*genai.Part{
					{Text: "I will read the file."},
					{FunctionCall: &genai.FunctionCall{
						ID:   "call-1",
						Name: "fs.read",
						Args: map[string]any{"path": "pkg/foo/bar.go"},
					}},
				},
			},
			FinishReason: genai.FinishReasonStop,
		}},
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     50,
			CandidatesTokenCount: 10,
		},
	}

	out := decodeGeminiResponse(resp)

	if out.Text != "I will read the file." {
		t.Errorf("Text = %q", out.Text)
	}
	if len(out.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(out.ToolCalls))
	}
	call := out.ToolCalls[0]
	if call.ID != "call-1" {
		t.Errorf("ID = %q, want call-1", call.ID)
	}
	if call.Name != "fs.read" {
		t.Errorf("Name = %q, want fs.read (dotted name preserved)", call.Name)
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil || args.Path != "pkg/foo/bar.go" {
		t.Errorf("Input = %s (err %v)", call.Input, err)
	}
	want := Usage{InputTokens: 50, OutputTokens: 10}
	if out.Usage != want {
		t.Errorf("Usage = %+v, want %+v", out.Usage, want)
	}
}

func TestDecodeGeminiResponseFunctionCallNoID(t *testing.T) {
	// When FunctionCall.ID is empty the adapter synthesises the name as the ID.
	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content: &genai.Content{
				Parts: []*genai.Part{{
					FunctionCall: &genai.FunctionCall{
						Name: "go.build",
						Args: map[string]any{},
					},
				}},
			},
			FinishReason: genai.FinishReasonStop,
		}},
	}
	out := decodeGeminiResponse(resp)
	if len(out.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(out.ToolCalls))
	}
	if out.ToolCalls[0].ID != "go.build" {
		t.Errorf("ID fallback = %q, want go.build", out.ToolCalls[0].ID)
	}
}

func TestDecodeGeminiResponseEmpty(t *testing.T) {
	// No candidates — should not panic.
	out := decodeGeminiResponse(&genai.GenerateContentResponse{})
	if out.Text != "" || len(out.ToolCalls) != 0 {
		t.Errorf("non-empty response for empty input: %+v", out)
	}
}

func TestDecodeGeminiResponseMaxTokens(t *testing.T) {
	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content:      &genai.Content{Parts: []*genai.Part{{Text: "truncated"}}},
			FinishReason: genai.FinishReasonMaxTokens,
		}},
	}
	out := decodeGeminiResponse(resp)
	if out.StopReason != "max_tokens" {
		t.Errorf("StopReason = %q, want max_tokens", out.StopReason)
	}
}

// Thinking-model regression tests — gemini-2.5-flash and similar models return
// parts with Thought=true that represent the model's internal reasoning chain.
// Those parts must never surface as visible text output or mask real content.

func TestDecodeGeminiResponseThoughtOnly(t *testing.T) {
	// A response composed entirely of thought parts (model reasoned but produced
	// no visible text and no function call) must yield an empty Response so the
	// llm handler fires an llm.failed event (G4: no silent dead ends).
	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content: &genai.Content{
				Role: "model",
				Parts: []*genai.Part{
					{Text: "Let me think step by step...", Thought: true},
					{Text: "I need to consider the trade-offs.", Thought: true},
				},
			},
			FinishReason: genai.FinishReasonStop,
		}},
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:        200,
			CandidatesTokenCount:    28,
			ThoughtsTokenCount:      28,
		},
	}
	out := decodeGeminiResponse(resp)
	if out.Text != "" {
		t.Errorf("thought-only response must yield empty Text, got %q", out.Text)
	}
	if len(out.ToolCalls) != 0 {
		t.Errorf("thought-only response must yield no ToolCalls, got %+v", out.ToolCalls)
	}
	if out.StopReason != "stop" {
		t.Errorf("StopReason = %q, want stop", out.StopReason)
	}
	// Usage is still forwarded — the tokens were spent on thinking.
	if out.Usage.InputTokens != 200 || out.Usage.OutputTokens != 28 {
		t.Errorf("Usage = %+v, want {Input:200 Output:28}", out.Usage)
	}
}

func TestDecodeGeminiResponseThoughtPlusFunctionCall(t *testing.T) {
	// A thinking model that emits a thought followed by a function call: the
	// thought must be dropped and the function call must be captured.
	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content: &genai.Content{
				Role: "model",
				Parts: []*genai.Part{
					{Text: "I should read the file first.", Thought: true},
					{FunctionCall: &genai.FunctionCall{
						ID:   "call-42",
						Name: "fs.read",
						Args: map[string]any{"path": "main.go"},
					}},
				},
			},
			FinishReason: genai.FinishReasonStop,
		}},
	}
	out := decodeGeminiResponse(resp)
	if out.Text != "" {
		t.Errorf("thought text must be dropped; Text = %q", out.Text)
	}
	if len(out.ToolCalls) != 1 {
		t.Fatalf("want 1 ToolCall, got %d: %+v", len(out.ToolCalls), out.ToolCalls)
	}
	if out.ToolCalls[0].Name != "fs.read" || out.ToolCalls[0].ID != "call-42" {
		t.Errorf("ToolCall = %+v", out.ToolCalls[0])
	}
}

func TestDecodeGeminiResponseThoughtPlusRealText(t *testing.T) {
	// A thinking model that emits a thought followed by a visible text answer:
	// the thought must be dropped and only the visible text must be captured.
	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content: &genai.Content{
				Role: "model",
				Parts: []*genai.Part{
					{Text: "Reasoning: 2 + 2 = 4 because...", Thought: true},
					{Text: "The answer is 4.", Thought: false},
				},
			},
			FinishReason: genai.FinishReasonStop,
		}},
	}
	out := decodeGeminiResponse(resp)
	if out.Text != "The answer is 4." {
		t.Errorf("Text = %q, want only the non-thought part", out.Text)
	}
	if len(out.ToolCalls) != 0 {
		t.Errorf("unexpected ToolCalls: %+v", out.ToolCalls)
	}
}
