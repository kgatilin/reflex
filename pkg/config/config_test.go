package config

import (
	"errors"
	"strings"
	"testing"
)

var known = map[string]bool{
	"llm_stub":          true,
	"tool_call":         true,
	"printer":           true,
	"unhandled_watcher": true,
}

func TestParseValid(t *testing.T) {
	src := `
settings:
  max_steps: 32
handlers:
  - name: brain
    type: llm_stub
    on: RequestReceived
    emits: [ToolCallProposed, AssistantMessageProposed, RequestHandled]
    config:
      rules:
        - match: "2+2"
          tool: calc
          args: "2+2"
  - name: calc-tool
    type: tool_call
    on: ToolCallProposed
    emits: [ToolResultObserved]
    config:
      tool: calc
`
	f, err := Parse([]byte(src), known)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Settings.MaxSteps != 32 {
		t.Errorf("max_steps = %d", f.Settings.MaxSteps)
	}
	if len(f.Handlers) != 2 {
		t.Errorf("expected 2 handlers, got %d", len(f.Handlers))
	}
	if f.Handlers[0].Config["rules"] == nil {
		t.Error("rules not parsed")
	}
}

func TestParseRejectsUnknownType(t *testing.T) {
	src := `
handlers:
  - name: weird
    type: not_a_thing
    on: X
`
	_, err := Parse([]byte(src), known)
	if !errors.Is(err, ErrUnknownHandlerType) {
		t.Fatalf("expected ErrUnknownHandlerType, got %v", err)
	}
}

func TestParseRejectsDuplicateName(t *testing.T) {
	src := `
handlers:
  - name: a
    type: printer
    on: X
  - name: a
    type: printer
    on: Y
`
	_, err := Parse([]byte(src), known)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate error, got %v", err)
	}
}

func TestParseRejectsMissingFields(t *testing.T) {
	cases := []string{
		`handlers: []`,
		`handlers: [{type: printer, on: X}]`,
		`handlers: [{name: a, on: X}]`,
		`handlers: [{name: a, type: printer}]`,
	}
	for i, c := range cases {
		if _, err := Parse([]byte(c), known); err == nil {
			t.Errorf("case %d: expected error, got nil", i)
		}
	}
}
