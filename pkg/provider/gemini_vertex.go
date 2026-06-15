package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"google.golang.org/genai"
)

// geminiVertex is the Vertex-AI Gemini adapter. It was added as the first
// bootstrap-agent task (doc 23 task 1a) after confirming that the GCP project
// has no Anthropic publisher access — the Vertex-Anthropic adapter shipped in
// stage 0 cannot be used in this deployment, so Gemini is the working hand.
//
// Registry key: "vertex" (bare transport, no family). Bindings of the form
// "vertex:gemini-2.5-flash" parse to {Transport: vertex, Family: "", Model:
// gemini-2.5-flash}, so Key() == "vertex". The factory is registered under
// that key in init().
//
// Auth: Application Default Credentials, picked up automatically by the genai
// SDK. One client is lazily built per {project, location} and reused — client
// construction is the expensive part.
//
// Function-name encoding: Gemini's FunctionDeclaration.Name accepts dots
// (documented: "a-z, A-Z, 0-9, or contain underscores, dots, colons and
// dashes"). Tool names are therefore passed through unchanged — no
// transcoding needed. The WireToolName / DottedToolName helpers are used by
// the Anthropic adapter which DOES forbid dots; we skip them here.
type geminiVertex struct {
	cfg Config

	mu     sync.Mutex
	client *genai.Client
	key    string
}

const defaultGeminiLocation = "global"

func init() {
	RegisterFactory("vertex", func(cfg Config) (Provider, error) {
		if cfg.Project == "" {
			return nil, fmt.Errorf("provider vertex(gemini): project is required")
		}
		if cfg.Location == "" {
			cfg.Location = defaultGeminiLocation
		}
		return &geminiVertex{cfg: cfg}, nil
	})
}

func (g *geminiVertex) Complete(ctx context.Context, req Request) (Response, error) {
	client, err := g.clientFor(ctx)
	if err != nil {
		return Response{}, err
	}
	contents, cfg, err := geminiParams(req)
	if err != nil {
		return Response{}, err
	}
	resp, err := client.Models.GenerateContent(ctx, req.Model, contents, cfg)
	if err != nil {
		return Response{}, fmt.Errorf("vertex-gemini generateContent: %w", err)
	}
	return decodeGeminiResponse(resp), nil
}

// geminiParams translates the neutral Request into genai contents + config.
// System prompt → GenerateContentConfig.SystemInstruction.
// Messages → []*genai.Content with role "user" or "model".
// Tools → genai.Tool with FunctionDeclarations; dots kept as-is (Gemini
// allows them in function names).
// Pure — unit-tested without a network.
func geminiParams(req Request) ([]*genai.Content, *genai.GenerateContentConfig, error) {
	maxTokens := int32(req.MaxTokens)
	if maxTokens <= 0 {
		maxTokens = 8192
	}

	cfg := &genai.GenerateContentConfig{
		MaxOutputTokens: maxTokens,
	}

	if req.System != "" {
		cfg.SystemInstruction = genai.NewContentFromText(req.System, genai.RoleUser)
	}

	if len(req.Tools) > 0 {
		decls, err := geminiFunctionDeclarations(req.Tools)
		if err != nil {
			return nil, nil, err
		}
		cfg.Tools = []*genai.Tool{{FunctionDeclarations: decls}}
	}

	var contents []*genai.Content
	for _, m := range req.Messages {
		role := genai.Role(genai.RoleUser)
		if m.Role == "assistant" {
			role = genai.Role(genai.RoleModel)
		}
		contents = append(contents, genai.NewContentFromText(m.Text, role))
	}

	return contents, cfg, nil
}

// geminiFunctionDeclarations translates the neutral ToolSchema slice into
// genai FunctionDeclarations. Gemini's wire format accepts JSON Schema
// directly via ParametersJsonSchema (any), so we unmarshal the raw schema
// bytes and pass the resulting map through — no re-encoding needed.
// Tool names are passed unchanged: dots are legal in Gemini function names.
func geminiFunctionDeclarations(tools []ToolSchema) ([]*genai.FunctionDeclaration, error) {
	decls := make([]*genai.FunctionDeclaration, 0, len(tools))
	for _, t := range tools {
		d := &genai.FunctionDeclaration{
			Name:        t.Name, // dots are accepted by Gemini; no transcoding
			Description: t.Description,
		}
		if len(t.InputSchema) > 0 {
			var schema any
			if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
				return nil, fmt.Errorf("provider: tool %q input_schema is not valid JSON: %w", t.Name, err)
			}
			d.ParametersJsonSchema = schema
		}
		decls = append(decls, d)
	}
	return decls, nil
}

// decodeGeminiResponse folds the SDK response into the neutral Response.
// Text parts are concatenated; function-call parts become ToolCalls (ID from
// the FunctionCall.ID field if set, otherwise synthesised from the name).
// Thought parts (part.Thought == true) are silently dropped — they are the
// model's internal reasoning chain, not content to return to the caller. A
// response composed entirely of thought parts (thinking model reasoning without
// a visible answer) produces empty Text and no ToolCalls, which the llm handler
// converts to an llm.failed event (G4 invariant: no silent dead ends).
// FinishReason is lowercased for consistency with the Anthropic stop_reason
// convention ("stop", "max_tokens", etc.).
// Usage is filled from UsageMetadata; CachedContentTokenCount maps to
// CacheReadTokens (Gemini does not expose a separate cache-creation count).
// Pure — unit-tested without a network.
func decodeGeminiResponse(resp *genai.GenerateContentResponse) Response {
	var (
		textBuf strings.Builder
		calls   []ToolCall
		reason  string
	)

	if len(resp.Candidates) > 0 {
		cand := resp.Candidates[0]
		reason = strings.ToLower(string(cand.FinishReason))
		if cand.Content != nil {
			for _, part := range cand.Content.Parts {
				switch {
				case part.Text != "" && !part.Thought:
					// Thought parts carry the model's reasoning trace; skip them
					// so they never surface as visible text output.
					textBuf.WriteString(part.Text)
				case part.FunctionCall != nil:
					fc := part.FunctionCall
					// Encode args map back to JSON so the neutral ToolCall.Input
					// is always a JSON object, matching the Anthropic adapter's contract.
					argBytes, _ := json.Marshal(fc.Args)
					id := fc.ID
					if id == "" {
						id = fc.Name
					}
					calls = append(calls, ToolCall{
						ID:    id,
						Name:  fc.Name, // already dotted; no decoding needed
						Input: json.RawMessage(argBytes),
					})
				}
			}
		}
	}

	var usage Usage
	if m := resp.UsageMetadata; m != nil {
		usage = Usage{
			InputTokens:     int64(m.PromptTokenCount),
			OutputTokens:    int64(m.CandidatesTokenCount),
			CacheReadTokens: int64(m.CachedContentTokenCount),
			ThoughtsTokens:  int64(m.ThoughtsTokenCount),
			// Gemini does not surface a separate cache-creation token count.
		}
	}

	return Response{
		Text:       textBuf.String(),
		ToolCalls:  calls,
		StopReason: reason,
		Usage:      usage,
	}
}

// clientFor lazily builds the Vertex-authenticated genai client. ADC is
// picked up automatically by the SDK; reflex never handles a bearer token.
// The client is cached per {project, location} for the process lifetime —
// client construction triggers credential discovery, which is expensive.
func (g *geminiVertex) clientFor(ctx context.Context) (*genai.Client, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := g.cfg.Project + "|" + g.cfg.Location
	if g.client != nil && g.key == key {
		return g.client, nil
	}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		Backend:  genai.BackendVertexAI,
		Project:  g.cfg.Project,
		Location: g.cfg.Location,
	})
	if err != nil {
		return nil, fmt.Errorf("vertex-gemini genai.NewClient (project=%s location=%s): %w", g.cfg.Project, g.cfg.Location, err)
	}
	g.client = client
	g.key = key
	return client, nil
}
