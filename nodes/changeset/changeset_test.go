package changeset

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kgatilin/reflex/engine"
)

// reqPayload mirrors the engine's (unexported) changeset request payload so the
// test can read the ops the bridge produced. engine.Op is exported.
type reqPayload struct {
	Ops       []engine.Op `json:"ops"`
	Principal string      `json:"principal"`
}

const sampleDoc = `
events:
  - {kind: task.new, terminal: true}
  - {kind: worker.done, terminal: true}
subscribers:
  - name: worker
    on: [task.new]
    emits: [worker.done]
    body: {kind: entry, config: {emit: worker.done}}
`

func react(t *testing.T, cfg Config, payload string) []engine.Emit {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	r, err := Factory(engine.Subscriber{Name: "bridge", BodyConfig: raw})
	if err != nil {
		t.Fatalf("Factory: %v", err)
	}
	emits, err := r.React(context.Background(), engine.Event{Payload: json.RawMessage(payload)}, nil)
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	return emits
}

// TestBridge_DocumentToChangesetRequest proves the bridge turns a topology
// document (a brain's "apply this topology" tool-call) into a single
// engine.KindChangesetRequested emit carrying the document's decls as add-ops.
func TestBridge_DocumentToChangesetRequest(t *testing.T) {
	payload, _ := json.Marshal(map[string]string{"document": sampleDoc})
	emits := react(t, Config{}, string(payload))

	if len(emits) != 1 {
		t.Fatalf("emits = %d, want 1", len(emits))
	}
	if emits[0].Kind != engine.KindChangesetRequested {
		t.Fatalf("emit kind = %q, want %q", emits[0].Kind, engine.KindChangesetRequested)
	}

	var rp reqPayload
	if err := json.Unmarshal(emits[0].Payload, &rp); err != nil {
		t.Fatalf("unmarshal request payload: %v", err)
	}
	if rp.Principal != defaultPrinc {
		t.Errorf("principal = %q, want %q", rp.Principal, defaultPrinc)
	}
	// The doc has 2 events + 1 subscriber → 3 add-ops.
	if len(rp.Ops) != 3 {
		t.Fatalf("ops = %d, want 3 (2 events + 1 subscriber)", len(rp.Ops))
	}
	var subOps, evOps int
	for _, op := range rp.Ops {
		if op.Verb != "add" {
			t.Errorf("op %q verb = %q, want add", op.Name, op.Verb)
		}
		switch op.Kind {
		case "subscriber":
			subOps++
		case "event":
			evOps++
		}
	}
	if subOps != 1 || evOps != 2 {
		t.Errorf("op kinds: subscriber=%d event=%d, want 1 and 2", subOps, evOps)
	}
}

// TestBridge_InlineObjectDocument proves the document field may be an inline JSON
// object (already structured), not only a YAML string.
func TestBridge_InlineObjectDocument(t *testing.T) {
	payload := `{"document": {"events": [{"kind": "x.k", "terminal": true}]}}`
	emits := react(t, Config{}, payload)
	if len(emits) != 1 || emits[0].Kind != engine.KindChangesetRequested {
		t.Fatalf("emits = %+v, want one changeset request", emits)
	}
	var rp reqPayload
	_ = json.Unmarshal(emits[0].Payload, &rp)
	if len(rp.Ops) != 1 || rp.Ops[0].Kind != "event" {
		t.Errorf("ops = %+v, want one event op", rp.Ops)
	}
}

// TestBridge_ParseFailureEmitsFail proves a malformed document yields the fail
// feedback kind (carrying the error) rather than a changeset request — the
// brain's signal to fix the document.
func TestBridge_ParseFailureEmitsFail(t *testing.T) {
	payload := `{"document": "this: : : not valid yaml: ["}`
	emits := react(t, Config{}, payload)
	if len(emits) != 1 {
		t.Fatalf("emits = %d, want 1", len(emits))
	}
	if emits[0].Kind != defaultFail {
		t.Fatalf("emit kind = %q, want %q", emits[0].Kind, defaultFail)
	}
	var body map[string]string
	if err := json.Unmarshal(emits[0].Payload, &body); err != nil || body["error"] == "" {
		t.Errorf("fail payload = %s, want a non-empty error field", emits[0].Payload)
	}
}

// TestBridge_ZeroDeclsRejected proves a well-formed YAML that matches NO reflex
// field (a foreign agent format) is rejected — not applied as a silent no-op. The
// fail message names the keys actually sent, so the brain can correct the format.
func TestBridge_ZeroDeclsRejected(t *testing.T) {
	foreign := "nodes:\n- agent:\n    tools: [bash, python]\n  name: worker\n"
	payload, _ := json.Marshal(map[string]string{"document": foreign})
	emits := react(t, Config{}, string(payload))
	if len(emits) != 1 || emits[0].Kind != defaultFail {
		t.Fatalf("emits = %+v, want one fail emit (0-decl document)", emits)
	}
	var body map[string]string
	_ = json.Unmarshal(emits[0].Payload, &body)
	if body["error"] == "" || !contains(body["error"], "nodes") {
		t.Errorf("fail error = %q, want it to name the foreign top-level key 'nodes'", body["error"])
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// TestBridge_MissingField proves a payload with no document field fails cleanly.
func TestBridge_MissingField(t *testing.T) {
	emits := react(t, Config{}, `{"something": "else"}`)
	if len(emits) != 1 || emits[0].Kind != defaultFail {
		t.Fatalf("emits = %+v, want one fail emit", emits)
	}
}
