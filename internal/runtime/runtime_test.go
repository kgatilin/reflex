package runtime

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/handler"
	"github.com/kgatilin/reflex/pkg/projection"
)

const calcYAML = `
handlers:
  - name: brain
    type: llm_stub
    on: RequestReceived
    emits: [ToolCallProposed]
    config:
      rules:
        - match: "2+2"
          action: tool_call
          tool: calc
          args: "2+2"
      fallback:
        action: reply_and_handle
        reply: "I don't know."
  - name: brain-after-tool
    type: llm_stub
    on: ToolResultObserved
    emits: [AssistantMessageProposed, RequestHandled]
    config:
      trigger_on: [ToolResultObserved]
      fallback:
        action: reply_and_handle
        reply: "The answer is {last_tool_result}"
  - name: calc-tool
    type: tool_call
    on: ToolCallProposed
    emits: [ToolResultObserved]
    config:
      tool: calc
  - name: out
    type: printer
    on: AssistantMessageProposed
`

func TestEndToEndCalcScenario(t *testing.T) {
	var buf bytes.Buffer
	prev := handler.SetPrinterOutput(&buf)
	t.Cleanup(func() { handler.SetPrinterOutput(prev) })

	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse([]byte(calcYAML), reg.Types())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b, err := Build(cfg, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	res, err := Run(context.Background(), b, "what is 2+2")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !res.State.Handled {
		t.Fatal("request was not handled")
	}
	if res.State.Unhandled {
		t.Fatal("unhandled flag set unexpectedly")
	}
	gotTypes := []string{}
	for _, e := range res.State.Events {
		// Filter out bus meta-events (Phase 1.6); this assertion is
		// about the user-level chain.
		switch e.Type {
		case projection.TypeEventDispatched,
			projection.TypeDrainQuiesced,
			projection.TypeHandlerFailed:
			continue
		}
		gotTypes = append(gotTypes, e.Type)
	}
	want := []string{
		projection.TypeRequestReceived,
		projection.TypeToolCallProposed,
		projection.TypeToolResultObserved,
		projection.TypeAssistantMessageProposed,
		projection.TypeRequestHandled,
	}
	if len(gotTypes) != len(want) {
		t.Fatalf("event types = %v, want %v", gotTypes, want)
	}
	for i := range want {
		if gotTypes[i] != want[i] {
			t.Fatalf("event[%d] = %s, want %s", i, gotTypes[i], want[i])
		}
	}
	if !strings.Contains(buf.String(), "The answer is 4") {
		t.Fatalf("printer output = %q", buf.String())
	}
}

const stallYAML = `
handlers:
  - name: hopeless
    type: llm_stub
    on: RequestReceived
    emits: [AssistantMessageProposed]
    config:
      fallback:
        action: reply
        reply: "I emit something but never RequestHandled."
  - name: out
    type: printer
    on: AssistantMessageProposed
  - name: watcher
    type: unhandled_watcher
    on: __noop__
`

func TestEndToEndStallScenarioFiresUnhandled(t *testing.T) {
	var buf bytes.Buffer
	prev := handler.SetPrinterOutput(&buf)
	t.Cleanup(func() { handler.SetPrinterOutput(prev) })

	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse([]byte(stallYAML), reg.Types())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b, err := Build(cfg, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	res, err := Run(context.Background(), b, "this will stall")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.State.Handled {
		t.Fatal("expected unhandled, but got handled")
	}
	if !res.State.Unhandled {
		t.Fatal("expected Unhandled flag set")
	}
	if !strings.Contains(res.State.UnhandledReason, "drain quiesced") {
		t.Fatalf("reason = %q", res.State.UnhandledReason)
	}
	// And bounded — the dispatcher must have terminated.
	if len(res.Events) > 20 {
		t.Fatalf("expected bounded events, got %d", len(res.Events))
	}
}
