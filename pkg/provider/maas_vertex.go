package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

type maasVertex struct {
	cfg    Config
	family string

	mu sync.Mutex
	ts oauth2.TokenSource
}

func init() {
	makeFactory := func(family string) Factory {
		return func(cfg Config) (Provider, error) {
			if cfg.Project == "" {
				return nil, fmt.Errorf("provider vertex:%s: project is required", family)
			}
			if cfg.Location == "" {
				cfg.Location = "us-central1"
			}
			return &maasVertex{
				cfg:    cfg,
				family: family,
			}, nil
		}
	}

	RegisterFactory("vertex:meta", makeFactory("meta"))
	RegisterFactory("vertex:deepseek-ai", makeFactory("deepseek-ai"))
	RegisterFactory("vertex:qwen", makeFactory("qwen"))
}

type maasMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []maasToolCall `json:"tool_calls,omitempty"`
}

type maasFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type maasTool struct {
	Type     string       `json:"type"`
	Function maasFunction `json:"function"`
}

type maasToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function maasFunctionCall `json:"function"`
}

type maasFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type maasRequest struct {
	Model     string        `json:"model"`
	Messages  []maasMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens,omitempty"`
	Tools     []maasTool    `json:"tools,omitempty"`
}

type maasResponse struct {
	Choices []struct {
		Message struct {
			Role      string         `json:"role"`
			Content   string         `json:"content"`
			ToolCalls []maasToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

func (v *maasVertex) Complete(ctx context.Context, req Request) (Response, error) {
	ts, err := v.tokenSource(ctx)
	if err != nil {
		return Response{}, err
	}
	token, err := ts.Token()
	if err != nil {
		return Response{}, fmt.Errorf("maas: get token: %w", err)
	}

	url := fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/endpoints/openapi/chat/completions",
		v.cfg.Location, v.cfg.Project, v.cfg.Location)

	maasReq, nameMap, err := v.buildRequest(req)
	if err != nil {
		return Response{}, err
	}

	reqBodyBytes, err := json.Marshal(maasReq)
	if err != nil {
		return Response{}, fmt.Errorf("maas marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBodyBytes))
	if err != nil {
		return Response{}, fmt.Errorf("maas new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token.AccessToken)

	httpResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("maas http request failed: %w", err)
	}
	defer httpResp.Body.Close()

	respBytes, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("maas read response body: %w", err)
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return Response{}, fmt.Errorf("maas HTTP error %d: %s", httpResp.StatusCode, string(respBytes))
	}

	var maasResp maasResponse
	if err := json.Unmarshal(respBytes, &maasResp); err != nil {
		return Response{}, fmt.Errorf("maas unmarshal response (status %d): %w (body: %s)", httpResp.StatusCode, err, string(respBytes))
	}

	return decodeMaasResponse(maasResp, nameMap), nil
}

func (v *maasVertex) buildRequest(req Request) (maasRequest, map[string]string, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 8192
	}

	var messages []maasMessage
	if req.System != "" {
		messages = append(messages, maasMessage{
			Role:    "system",
			Content: req.System,
		})
	}

	for _, m := range req.Messages {
		role := m.Role
		if role == "" {
			role = "user"
		}
		messages = append(messages, maasMessage{
			Role:    role,
			Content: m.Text,
		})
	}

	nameMap := make(map[string]string)
	var tools []maasTool
	for _, t := range req.Tools {
		encodedName := WireToolName(t.Name)
		nameMap[encodedName] = t.Name

		var params json.RawMessage
		if len(t.InputSchema) > 0 {
			params = t.InputSchema
		}
		tools = append(tools, maasTool{
			Type: "function",
			Function: maasFunction{
				Name:        encodedName,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}

	modelParam := v.family + "/" + req.Model

	maasReq := maasRequest{
		Model:     modelParam,
		Messages:  messages,
		MaxTokens: maxTokens,
		Tools:     tools,
	}

	return maasReq, nameMap, nil
}

func decodeMaasResponse(resp maasResponse, nameMap map[string]string) Response {
	var (
		textBuf strings.Builder
		calls   []ToolCall
		reason  string
	)

	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		reason = choice.FinishReason
		textBuf.WriteString(choice.Message.Content)

		for _, tc := range choice.Message.ToolCalls {
			if tc.Type == "function" {
				origName, ok := nameMap[tc.Function.Name]
				if !ok {
					origName = DottedToolName(tc.Function.Name)
				}
				calls = append(calls, ToolCall{
					ID:    tc.ID,
					Name:  origName,
					Input: json.RawMessage(tc.Function.Arguments),
				})
			}
		}
	}

	return Response{
		Text:       textBuf.String(),
		ToolCalls:  calls,
		StopReason: reason,
		Usage: Usage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		},
	}
}

func (v *maasVertex) tokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.ts != nil {
		return v.ts, nil
	}
	creds, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, fmt.Errorf("maas: find default credentials: %w", err)
	}
	v.ts = creds.TokenSource
	return v.ts, nil
}
