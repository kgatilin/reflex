package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/handler"
	"github.com/kgatilin/reflex/pkg/projection"
)

// TestLoopExampleEmitsLoopExhausted runs examples/loop.yaml end-to-end and
// asserts that the dispatcher fires the bouncer at most max_iterations
// (= 2) times, then emits LoopExhausted (terminal) and quiesces without
// orphan diagnostics.
func TestLoopExampleEmitsLoopExhausted(t *testing.T) {
	path := loopExamplePath(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse(data, reg.Types())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b, err := Build(cfg, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	res, err := Run(context.Background(), b, "start")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Count bouncer fires (each fire emits one PingEvent sourced from
	// "bouncer") and assert <= max_iterations (= 2 in the config).
	bouncerFires := 0
	loopExhausted := false
	for _, e := range res.Events {
		if e.Source == "bouncer" && e.Type == "PingEvent" {
			bouncerFires++
		}
		if e.Type == projection.TypeLoopExhausted {
			loopExhausted = true
			if !e.Terminal {
				t.Fatal("LoopExhausted must be terminal")
			}
		}
	}
	if bouncerFires > 2 {
		t.Fatalf("bouncer fired %d times, max_iterations=2", bouncerFires)
	}
	if bouncerFires == 0 {
		t.Fatal("bouncer never fired — the loop isn't being entered")
	}
	if !loopExhausted {
		t.Fatal("expected LoopExhausted in the event log")
	}
	if res.State.Unhandled {
		t.Fatalf("LoopExhausted should leave the request handled, got unhandled: %s", res.State.UnhandledReason)
	}
	if !res.State.Handled {
		t.Fatal("LoopExhausted should mark the request handled (projection sets Handled)")
	}

	// No orphan diagnostics for non-terminal events in the loop path.
	for _, e := range res.Events {
		if e.Type == projection.TypeEventOrphaned {
			// An orphan IS expected if a PongEvent has no descendant — and
			// that's the case for the last PongEvent before LoopExhausted
			// is emitted (LoopExhausted is caused_by the trigger event, so
			// the PongEvent it skipped DOES have a child). Concretely, the
			// chain is:
			//   PongEvent_n -> (bouncer skipped) -> LoopExhausted (caused_by PongEvent_n)
			// so PongEvent_n has a child. We accept any orphan here only
			// for diagnostic completeness — but in the well-formed case
			// there should be none.
			t.Fatalf("unexpected orphan in loop run: %+v", e)
		}
	}
}

func loopExamplePath(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		p := filepath.Join(cwd, "examples", "loop.yaml")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		cwd = filepath.Dir(cwd)
	}
	t.Fatal("can't find examples/loop.yaml")
	return ""
}
