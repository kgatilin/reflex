package daemon

import (
	"encoding/json"
	"testing"

	"github.com/kgatilin/reflex/engine"
)

// TestMirrorLLMEmits threads the subscriber's emit allowlist into the llm body
// descriptor. The live drive exposed the gap: the body advertises its Emits as
// the model's function menu, but the body resolver receives only the serialized
// config — not the subscriber's Emits — so without this the model gets no tools
// and narrates prose instead of acting.
func TestMirrorLLMEmits(t *testing.T) {
	emits := []string{"tool.fs.read.call", "claim.complete", "llm.message", "llm.usage"}
	in := []engine.Decl{
		engine.Subscriber{
			Name:       "brain",
			On:         []string{"request.received"},
			In:         "request",
			Emits:      emits,
			BodyKind:   "llm",
			BodyConfig: json.RawMessage(`{"model":"vertex:gemini-2.5-pro","answer":"llm.message"}`),
		},
		// a non-llm subscriber and an llm seat that already names its own emits are
		// both passed through untouched.
		engine.Subscriber{Name: "verifier", On: []string{"claim.complete"}, In: "request", Emits: []string{"state.updated.status"}, BodyKind: "verifier"},
		engine.Subscriber{
			Name: "explicit", On: []string{"x"}, In: "request", Emits: []string{"a", "b"},
			BodyKind:   "llm",
			BodyConfig: json.RawMessage(`{"emits":["only.this"]}`),
		},
	}

	out, err := mirrorLLMEmits(in)
	if err != nil {
		t.Fatalf("mirrorLLMEmits: %v", err)
	}

	// brain: its config gained emits == the subscriber allowlist, model preserved.
	brain := out[0].(engine.Subscriber)
	var cfg struct {
		Model  string   `json:"model"`
		Answer string   `json:"answer"`
		Emits  []string `json:"emits"`
	}
	if err := json.Unmarshal(brain.BodyConfig, &cfg); err != nil {
		t.Fatalf("brain config: %v", err)
	}
	if cfg.Model != "vertex:gemini-2.5-pro" || cfg.Answer != "llm.message" {
		t.Errorf("mirroring clobbered existing config: %+v", cfg)
	}
	if len(cfg.Emits) != len(emits) {
		t.Fatalf("brain config emits = %v, want the subscriber allowlist %v", cfg.Emits, emits)
	}
	for i, k := range emits {
		if cfg.Emits[i] != k {
			t.Errorf("brain config emits[%d] = %q, want %q", i, cfg.Emits[i], k)
		}
	}

	// verifier (non-llm): untouched.
	if v := out[1].(engine.Subscriber); v.BodyKind != "verifier" || v.BodyConfig != nil {
		t.Errorf("non-llm subscriber was modified: %+v", v)
	}

	// explicit llm seat: its own emits are preserved, NOT overwritten.
	var ecfg struct {
		Emits []string `json:"emits"`
	}
	if err := json.Unmarshal(out[2].(engine.Subscriber).BodyConfig, &ecfg); err != nil {
		t.Fatalf("explicit config: %v", err)
	}
	if len(ecfg.Emits) != 1 || ecfg.Emits[0] != "only.this" {
		t.Errorf("explicit llm seat emits overwritten: %v, want [only.this]", ecfg.Emits)
	}
}
