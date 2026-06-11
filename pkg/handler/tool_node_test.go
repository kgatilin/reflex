package handler

import (
	"context"
	"testing"

	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
)

func newTestToolNode(t *testing.T) interface {
	React(context.Context, event.Event, []event.Event) ([]event.Event, error)
} {
	t.Helper()
	sub, err := newToolNode(config.HandlerConfig{
		Name: "calc", Type: "tool_node", On: "tool.calc.call",
		Config: map[string]any{"tool": "calc"},
	})
	if err != nil {
		t.Fatalf("newToolNode: %v", err)
	}
	return sub
}

func TestToolNodeSuccessEmitsResult(t *testing.T) {
	sub := newTestToolNode(t)
	ev := event.Event{
		Type:    "tool.calc.call",
		Payload: jsonRaw(map[string]string{"args": "161+11"}),
	}
	out, err := sub.React(context.Background(), ev, nil)
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	if len(out) != 1 || out[0].Type != "tool.calc.result" {
		t.Fatalf("expected tool.calc.result, got %+v", out)
	}
	if out[0].Terminal {
		t.Fatal("tool.calc.result must be non-terminal")
	}
	var p struct {
		Result string `json:"result"`
	}
	_ = out[0].PayloadAs(&p)
	if p.Result != "172" {
		t.Fatalf("result = %q, want 172", p.Result)
	}
}

func TestToolNodeFailureEmitsFailedNotGoError(t *testing.T) {
	sub := newTestToolNode(t)
	// calc cannot parse a non-arithmetic argument; the tool returns an error,
	// which the node must turn into a tool.calc.failed EVENT, not a Go error.
	ev := event.Event{
		Type:    "tool.calc.call",
		Payload: jsonRaw(map[string]string{"args": "not an expression"}),
	}
	out, err := sub.React(context.Background(), ev, nil)
	if err != nil {
		t.Fatalf("React returned a Go error (aborts the drain): %v", err)
	}
	if len(out) != 1 || out[0].Type != "tool.calc.failed" {
		t.Fatalf("expected tool.calc.failed, got %+v", out)
	}
	if out[0].Terminal {
		t.Fatal("tool.calc.failed must be non-terminal")
	}
	var p struct {
		Error string `json:"error"`
	}
	_ = out[0].PayloadAs(&p)
	if p.Error == "" {
		t.Fatal("failed event must carry an error message")
	}
}

func TestToolNodeRequiresTool(t *testing.T) {
	_, err := newToolNode(config.HandlerConfig{
		Name: "calc", Type: "tool_node", On: "tool.calc.call",
	})
	if err == nil {
		t.Fatal("expected error when tool missing")
	}
}

func TestToolNodeUnknownTool(t *testing.T) {
	_, err := newToolNode(config.HandlerConfig{
		Name: "x", Type: "tool_node", On: "tool.x.call",
		Config: map[string]any{"tool": "frobnicate"},
	})
	if err == nil {
		t.Fatal("expected error for unknown builtin tool")
	}
}
