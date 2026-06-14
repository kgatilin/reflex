package main

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/kgatilin/reflex/pkg/plugin"
)

// fakeRun builds a pytest with an injected runner so classification is testable
// without pytest installed.
func fakePytest(out string, exit int, runErr error) *pytest {
	return &pytest{root: ".", run: func(string, []string) (string, int, error) { return out, exit, runErr }}
}

func TestPytest_Classification(t *testing.T) {
	cases := []struct {
		name     string
		out      string
		exit     int
		runErr   error
		wantKind string
		wantPass bool
	}{
		{"all pass", "2 passed", 0, nil, "tool.py.test.result", true},
		{"tests failed", "1 failed", 1, nil, "tool.py.test.result", false},
		{"collection error", "errors during collection", 2, nil, "tool.py.test.failed", false},
		{"no tests", "no tests ran", 5, nil, "tool.py.test.failed", false},
		{"binary missing", "", -1, errors.New("exec: pytest not found"), "tool.py.test.failed", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := fakePytest(tc.out, tc.exit, tc.runErr)
			emits, err := p.handle(plugin.Event{Kind: "tool.py.test.call", Payload: json.RawMessage(`{"args":["-q"]}`)})
			if err != nil {
				t.Fatalf("handle: %v", err)
			}
			if len(emits) != 1 || emits[0].Kind != tc.wantKind {
				t.Fatalf("emits = %+v; want one %s", emits, tc.wantKind)
			}
			if tc.wantKind == "tool.py.test.result" {
				var r struct {
					Passed bool `json:"passed"`
				}
				if err := json.Unmarshal(emits[0].Payload, &r); err != nil {
					t.Fatal(err)
				}
				if r.Passed != tc.wantPass {
					t.Errorf("passed = %v, want %v", r.Passed, tc.wantPass)
				}
			}
		})
	}
}

func TestPytest_UnknownKind(t *testing.T) {
	p := fakePytest("", 0, nil)
	if _, err := p.handle(plugin.Event{Kind: "tool.py.test.nope"}); err == nil {
		t.Error("unknown kind returned nil error")
	}
}
