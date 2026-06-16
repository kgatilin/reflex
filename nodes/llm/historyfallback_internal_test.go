package llm

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kgatilin/reflex/engine"
	"github.com/kgatilin/reflex/pkg/provider"
)

// recProv records the system + message count of the request the body builds.
type recProv struct {
	sys  string
	msgs int
}

func (r *recProv) Complete(_ context.Context, req provider.Request) (provider.Response, error) {
	r.sys = req.System
	r.msgs = len(req.Messages)
	return provider.Response{StopReason: "end"}, nil
}

type fakeHist struct{}

func (fakeHist) System() string { return "WORKER SYSTEM PROMPT" }
func (fakeHist) Messages() []provider.Message {
	return []provider.Message{{Role: "user", Text: "the task"}}
}

// histViews returns a History only for the projection named `name`; everything
// else is the null object (nil) — including the default <node>.history name.
type histViews struct{ name string }

func (h histViews) Value(n string) any {
	if n == h.name {
		return fakeHist{}
	}
	return nil
}
func (histViews) KV(string) engine.KV                   { return nil }
func (histViews) Log(string) []engine.Event             { return nil }
func (histViews) Schema(string) (json.RawMessage, bool) { return nil, false }

// TestBody_HistoryFallbackToReads proves the history-wiring footgun is fixed: an
// llm node named "worker" whose history projection is named "history" (NOT the
// default "worker.history") — but listed in reads — still resolves its history, so
// the model gets its system prompt + task. Without the fallback the body looks up
// "worker.history", finds nothing, and the worker fires with an EMPTY system and no
// messages (exactly the architect-built worker that made zero tool calls).
func TestBody_HistoryFallbackToReads(t *testing.T) {
	cfg := Config{Name: "worker"}.defaults() // HistoryName defaults to "worker.history"
	cfg.historyReads = []string{"history"}   // the actual projection name, via reads
	rp := &recProv{}

	r := body(cfg, rp)
	if _, err := r.React(context.Background(), engine.Event{}, histViews{name: "history"}); err != nil {
		t.Fatalf("React: %v", err)
	}
	if rp.sys != "WORKER SYSTEM PROMPT" {
		t.Fatalf("system = %q, want the fallback history's system (it was not resolved)", rp.sys)
	}
	if rp.msgs == 0 {
		t.Fatal("messages = 0 — the fallback history's task message was not used")
	}
}

// TestBody_NoHistoryIsEmpty is the control: with neither the default name nor any
// resolvable read, the body sends an empty system + no messages (the broken state).
func TestBody_NoHistoryIsEmpty(t *testing.T) {
	cfg := Config{Name: "worker"}.defaults()
	cfg.historyReads = []string{"topology.draft"} // a non-history read → never resolves
	rp := &recProv{}

	r := body(cfg, rp)
	if _, err := r.React(context.Background(), engine.Event{}, histViews{name: "history"}); err != nil {
		t.Fatalf("React: %v", err)
	}
	if rp.sys != "" || rp.msgs != 0 {
		t.Fatalf("system=%q msgs=%d, want empty (no resolvable history)", rp.sys, rp.msgs)
	}
}
