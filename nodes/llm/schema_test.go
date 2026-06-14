package llm_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kgatilin/reflex/engine"
	"github.com/kgatilin/reflex/nodes/llm"
	"github.com/kgatilin/reflex/pkg/provider"
)

// capturing records the last Request so a test can inspect the advertised tools.
type capturing struct{ last provider.Request }

func (c *capturing) Complete(_ context.Context, req provider.Request) (provider.Response, error) {
	c.last = req
	return provider.Response{Usage: provider.Usage{InputTokens: 1, OutputTokens: 1}}, nil
}

// schemaViews is a Views whose Schema returns canned catalog schemas; the other
// surfaces are null objects (no history → empty system/messages).
type schemaViews struct{ schemas map[string]json.RawMessage }

func (schemaViews) Value(string) any          { return nil }
func (schemaViews) KV(string) engine.KV       { return nil }
func (schemaViews) Log(string) []engine.Event { return nil }
func (v schemaViews) Schema(kind string) (json.RawMessage, bool) {
	s, ok := v.schemas[kind]
	return s, ok
}

// TestBodyAdvertisesCatalogSchemas proves the llm body turns its Emits into
// advertised functions and fills each function's InputSchema from the catalog
// (Views.Schema) — no per-tool wiring. The answer kind is not advertised as a
// function (it is synthesised from text).
func TestBodyAdvertisesCatalogSchemas(t *testing.T) {
	readSchema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)

	cap := &capturing{}
	node, _ := llm.NewWithProvider(llm.Config{
		Name:        "brain",
		On:          []string{"request.received"},
		Emits:       []string{"tool.fs.read.call", "task.answered"},
		Answer:      "task.answered",
		HistoryName: "brain.history",
		Model:       "stub",
	}, cap)

	views := schemaViews{schemas: map[string]json.RawMessage{
		"tool.fs.read.call": readSchema,
		// task.answered intentionally absent — it is the answer kind, not a tool.
	}}
	if _, err := node.Body.React(context.Background(), engine.Event{}, views); err != nil {
		t.Fatalf("React: %v", err)
	}

	tools := cap.last.Tools
	if len(tools) != 1 {
		t.Fatalf("advertised %d tools, want 1 (Emits minus the answer kind): %+v", len(tools), tools)
	}
	if tools[0].Name != "tool.fs.read.call" {
		t.Fatalf("tool name = %q, want tool.fs.read.call", tools[0].Name)
	}
	if string(tools[0].InputSchema) != string(readSchema) {
		t.Errorf("InputSchema = %s; want the catalog schema %s", tools[0].InputSchema, readSchema)
	}
}
