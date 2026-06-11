package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/vertex"
)

// anthropicVertex is the stage-0 adapter: Claude models served through
// Vertex AI, authenticated via Application Default Credentials — reflex never
// handles a bearer token. One client is built lazily per {project, location}
// and reused for the process lifetime (credential discovery is the expensive
// part).
type anthropicVertex struct {
	cfg Config

	mu     sync.Mutex
	client *anthropic.Client
	key    string
}

const defaultAnthropicLocation = "us-east5"

func init() {
	RegisterFactory("vertex:anthropic", func(cfg Config) (Provider, error) {
		if cfg.Project == "" {
			return nil, fmt.Errorf("provider vertex:anthropic: project is required")
		}
		if cfg.Location == "" || cfg.Location == "global" {
			// Anthropic on Vertex is regional; "global" is the Gemini
			// default and not a valid Claude serving location.
			cfg.Location = defaultAnthropicLocation
		}
		return &anthropicVertex{cfg: cfg}, nil
	})
}

func (a *anthropicVertex) Complete(ctx context.Context, req Request) (Response, error) {
	client, err := a.clientFor(ctx)
	if err != nil {
		return Response{}, err
	}
	params, err := anthropicParams(req)
	if err != nil {
		return Response{}, err
	}
	msg, err := client.Messages.New(ctx, params)
	if err != nil {
		return Response{}, fmt.Errorf("vertex-anthropic messages.new: %w", err)
	}
	return decodeAnthropicMessage(msg), nil
}

// anthropicParams translates the neutral request into the SDK's wire params.
// Pure — unit-tested without a network.
func anthropicParams(req Request) (anthropic.MessageNewParams, error) {
	maxTokens := int64(req.MaxTokens)
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: maxTokens,
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.System}}
	}
	for _, m := range req.Messages {
		block := anthropic.NewTextBlock(m.Text)
		switch m.Role {
		case "assistant":
			params.Messages = append(params.Messages, anthropic.NewAssistantMessage(block))
		default:
			params.Messages = append(params.Messages, anthropic.NewUserMessage(block))
		}
	}
	for _, t := range req.Tools {
		schema, err := anthropicInputSchema(t)
		if err != nil {
			return anthropic.MessageNewParams{}, err
		}
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        WireToolName(t.Name),
			Description: anthropic.String(t.Description),
			InputSchema: schema,
		}})
	}
	return params, nil
}

// anthropicInputSchema lifts a raw JSON Schema object into the SDK's typed
// input-schema param (properties + required; the object type is implied).
func anthropicInputSchema(t ToolSchema) (anthropic.ToolInputSchemaParam, error) {
	var raw struct {
		Properties json.RawMessage `json:"properties"`
		Required   []string        `json:"required"`
	}
	if len(t.InputSchema) > 0 {
		if err := json.Unmarshal(t.InputSchema, &raw); err != nil {
			return anthropic.ToolInputSchemaParam{}, fmt.Errorf("provider: tool %q input_schema is not a JSON object: %w", t.Name, err)
		}
	}
	out := anthropic.ToolInputSchemaParam{Required: raw.Required}
	if len(raw.Properties) > 0 {
		var props map[string]any
		if err := json.Unmarshal(raw.Properties, &props); err != nil {
			return anthropic.ToolInputSchemaParam{}, fmt.Errorf("provider: tool %q properties: %w", t.Name, err)
		}
		out.Properties = props
	}
	return out, nil
}

// decodeAnthropicMessage folds the SDK response into the neutral Response:
// text blocks concatenate, tool_use blocks become structured ToolCalls with
// dotted names restored. Pure — unit-tested without a network.
func decodeAnthropicMessage(msg *anthropic.Message) Response {
	var (
		text  strings.Builder
		calls []ToolCall
	)
	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.AsText().Text)
		case "tool_use":
			tu := block.AsToolUse()
			calls = append(calls, ToolCall{
				ID:    tu.ID,
				Name:  DottedToolName(tu.Name),
				Input: tu.Input,
			})
		}
	}
	return Response{
		Text:       text.String(),
		ToolCalls:  calls,
		StopReason: string(msg.StopReason),
		Usage: Usage{
			InputTokens:         msg.Usage.InputTokens,
			OutputTokens:        msg.Usage.OutputTokens,
			CacheReadTokens:     msg.Usage.CacheReadInputTokens,
			CacheCreationTokens: msg.Usage.CacheCreationInputTokens,
		},
	}
}

// clientFor lazily builds the Vertex-authenticated client. The SDK's vertex
// option panics on credential failure; recover converts that into an
// ordinary error so a misconfigured environment surfaces as llm.failed, not
// a crashed daemon.
func (a *anthropicVertex) clientFor(ctx context.Context) (client *anthropic.Client, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := a.cfg.Project + "|" + a.cfg.Location
	if a.client != nil && a.key == key {
		return a.client, nil
	}
	defer func() {
		if r := recover(); r != nil {
			client, err = nil, fmt.Errorf("vertex-anthropic auth (project=%s location=%s): %v", a.cfg.Project, a.cfg.Location, r)
		}
	}()
	c := anthropic.NewClient(vertex.WithGoogleAuth(ctx, a.cfg.Location, a.cfg.Project))
	a.client = &c
	a.key = key
	return a.client, nil
}
