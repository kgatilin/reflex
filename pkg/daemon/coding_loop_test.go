package daemon_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kgatilin/reflex/engine"
	"github.com/kgatilin/reflex/pkg/daemon"
	"github.com/kgatilin/reflex/pkg/topology"
)

// codingLoopDoc is the doc-29 §4b coding-agent loop with the model and the hands
// replaced by deterministic stand-ins, so the LOOP + VERIFICATION + TERMINAL
// DRIVE are exercised on the real engine without a provider or plugins:
//
//   - the "brain" is an entry body that always (re-)claims complete — it stands
//     in for the model, driving the loop on request.received and on every
//     verify.failed, bounded by the request scope's claim.complete budget;
//   - the verifier runs verifierCmd as the agreed check — exit 0 ⇒ verified
//     done, non-zero ⇒ verify.failed back into the loop;
//   - the terminal gate turns a terminal status into request.terminal;
//   - fail-on-exhaust maps the budget backstop to a failed status.
//
// This is the committable proof that completion is a verified state and that the
// loop terminates either by verification (green) or by the budget backstop (red).
func codingLoopDoc(verifierCmd []string, claimBudget int) topology.Document {
	return topology.Document{
		Scopes: []topology.ScopeSpec{
			{Name: "request", Root: "request.received", Budget: map[string]int{"claim.complete": claimBudget}},
		},
		Events: []topology.EventSpec{
			{Kind: "task.new"},
			{Kind: "request.received"},
			{Kind: "claim.complete"},
			{Kind: "verify.failed"},
			{Kind: "state.updated.status"},
			{Kind: "request.terminal"},
		},
		Subscribers: []topology.SubscriberSpec{
			{Name: "resolver", On: []string{"task.new"}, In: "global", Emits: []string{"request.received"},
				Body: topology.BodySpec{Kind: "entry", Config: map[string]any{"emit": "request.received"}}},
			{Name: "brain", On: []string{"request.received", "verify.failed"}, In: "request",
				Emits: []string{"claim.complete"},
				Body:  topology.BodySpec{Kind: "entry", Config: map[string]any{"emit": "claim.complete"}}},
			{Name: "verifier", On: []string{"claim.complete"}, In: "request",
				Emits: []string{"state.updated.status", "verify.failed"},
				Body:  topology.BodySpec{Kind: "verifier", Config: map[string]any{"command": verifierCmd}}},
			// Terminal machinery lives in the parent (global) cone — a scope's
			// closure/exhaust facts exit the cone they seal, and a global watcher's
			// emit is placed back in the trigger's cone by causality. terminal-on-done
			// drives the terminal off the VERIFIED status; terminal-on-exhaust is the
			// budget backstop, mapping exhaustion straight to a failed terminal.
			{Name: "terminal-on-done", On: []string{"state.updated.status"}, In: "global",
				Emits: []string{"request.terminal"},
				Body:  topology.BodySpec{Kind: "gate", Config: map[string]any{"emit": "request.terminal", "when": []string{"done"}}}},
			{Name: "terminal-on-exhaust", On: []string{"scope.request.budget_exhausted"}, In: "global",
				Emits: []string{"request.terminal"},
				Body:  topology.BodySpec{Kind: "entry", Config: map[string]any{"emit": "request.terminal", "payload": map[string]any{"status": "failed"}}}},
			{Name: "terminal", On: []string{"request.terminal"}, In: "global"},
			{Name: "lifecycle", On: []string{"scope.request.closed"}, In: "global"},
		},
	}
}

func countKinds(events []engine.Event) map[string]int {
	m := map[string]int{}
	for _, ev := range events {
		m[engine.KindOf(ev)]++
	}
	return m
}

// terminalStatus returns the status carried by the single request.terminal event,
// failing if there is not exactly one.
func terminalStatus(t *testing.T, events []engine.Event) string {
	t.Helper()
	var statuses []string
	for _, ev := range events {
		if engine.KindOf(ev) != "request.terminal" {
			continue
		}
		var p struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		statuses = append(statuses, p.Status)
	}
	if len(statuses) != 1 {
		t.Fatalf("request.terminal count = %d (%v), want exactly 1", len(statuses), statuses)
	}
	return statuses[0]
}

// TestCodingLoop_VerifiedGreenDrivesTerminalDone proves the happy path on the
// real engine: task.new resolves into the request scope, the brain claims
// complete, the verifier's agreed check passes, and the VERIFIED done status
// drives request.terminal exactly once — then the scope closes. Done is a value
// the verifier wrote, not a claim.
func TestCodingLoop_VerifiedGreenDrivesTerminalDone(t *testing.T) {
	ctx := context.Background()
	d := daemon.New()

	if err := d.Apply(ctx, codingLoopDoc([]string{"sh", "-c", "exit 0"}, 4)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	produced, err := d.Emit(ctx, "task.new", []byte(`{"task":"fix the bug"}`), true)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	if status := terminalStatus(t, produced); status != "done" {
		t.Errorf("terminal status = %q, want done (verified green)", status)
	}
	counts := countKinds(produced)
	if counts["verify.failed"] != 0 {
		t.Errorf("verify.failed = %d, want 0 (the check passed first try)", counts["verify.failed"])
	}
	if counts["scope.request.closed"] != 1 {
		t.Errorf("scope.request.closed = %d, want 1 (G6)", counts["scope.request.closed"])
	}
	if counts["scope.request.budget_exhausted"] != 0 {
		t.Errorf("budget_exhausted = %d, want 0 (terminated by verification, not the backstop)", counts["scope.request.budget_exhausted"])
	}
}

// TestCodingLoop_RedLoopsUntilBudgetThenFails proves the failure path: the check
// never passes, so each claim drives a verify.failed back into the loop; the
// brain re-claims until the request scope's claim.complete budget caps it, which
// fires budget_exhausted once, which maps to a failed status that drives
// request.terminal. The loop terminates by the backstop, not by quiescence.
func TestCodingLoop_RedLoopsUntilBudgetThenFails(t *testing.T) {
	ctx := context.Background()
	d := daemon.New()

	const budget = 2
	if err := d.Apply(ctx, codingLoopDoc([]string{"sh", "-c", "exit 1"}, budget)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	produced, err := d.Emit(ctx, "task.new", []byte(`{"task":"unfixable"}`), true)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	if status := terminalStatus(t, produced); status != "failed" {
		t.Errorf("terminal status = %q, want failed (budget backstop)", status)
	}
	counts := countKinds(produced)
	if counts["verify.failed"] != budget {
		t.Errorf("verify.failed = %d, want %d (one per dispatched claim until the budget caps)", counts["verify.failed"], budget)
	}
	if counts["scope.request.budget_exhausted"] != 1 {
		t.Errorf("budget_exhausted = %d, want 1 (fired once at the cap)", counts["scope.request.budget_exhausted"])
	}
	if counts["scope.request.closed"] != 1 {
		t.Errorf("scope.request.closed = %d, want 1 (G6)", counts["scope.request.closed"])
	}
}
