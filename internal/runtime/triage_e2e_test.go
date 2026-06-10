package runtime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/handler"
	"github.com/kgatilin/reflex/pkg/projection"
)

const triageYAML = `
settings:
  max_steps: 64
handlers:
  - name: parse
    type: parse_target
    on: RequestReceived
  - name: fetch_comments
    type: gh_query
    on: TargetParsed
    config:
      path: comments
  - name: fetch_timeline
    type: gh_query
    on: TargetParsed
    config:
      path: timeline
  - name: classify
    type: triage_rules
    on: GhQueryResult
    config:
      now: "2026-06-07T12:00:00Z"
  - name: announce
    type: printer
    on: TriageDecided
    config:
      prefix: "triage: "
      field: reason
  - name: finalize
    type: terminator
    on: TriageDecided
  - name: watcher
    type: unhandled_watcher
    on: __noop__
`

// stubRunner serves canned JSON for gh api calls based on the path suffix.
type stubRunner struct {
	files map[string][]byte // suffix -> file contents
	fail  bool
}

func (s *stubRunner) Run(name string, args ...string) ([]byte, []byte, error) {
	if s.fail {
		return nil, []byte("gh: Not Found (HTTP 404)"), errors.New("exit status 1")
	}
	// Match the last arg containing a known suffix.
	for _, a := range args {
		for suffix, body := range s.files {
			if strings.HasSuffix(a, suffix) {
				return body, nil, nil
			}
		}
	}
	return nil, []byte("no fixture for " + strings.Join(args, " ")), errors.New("no fixture")
}

func loadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func TestTriageEndToEndStuckOnArchai114(t *testing.T) {
	var buf bytes.Buffer
	prev := handler.SetPrinterOutput(&buf)
	t.Cleanup(func() { handler.SetPrinterOutput(prev) })

	fixDir := filepath.Join("..", "..", "pkg", "handler", "testdata")
	stub := &stubRunner{files: map[string][]byte{
		"/comments": loadFile(t, filepath.Join(fixDir, "archai_114_comments.json")),
		"/timeline": loadFile(t, filepath.Join(fixDir, "archai_114_timeline.json")),
	}}
	prevRunner := handler.SetDefaultGhRunner(stub)
	t.Cleanup(func() { handler.SetDefaultGhRunner(prevRunner) })

	reg := handler.BuiltinRegistry()
	cfg, err := config.Parse([]byte(triageYAML), reg.Types())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b, err := Build(cfg, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	res, err := Run(context.Background(), b, "archai#114")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !res.State.Handled {
		t.Fatal("expected RequestHandled")
	}
	if !strings.Contains(buf.String(), "STUCK") {
		t.Fatalf("printer output = %q, want STUCK", buf.String())
	}

	gotTriage := false
	for _, e := range res.Events {
		if e.Type == projection.TypeTriageDecided {
			gotTriage = true
		}
		if e.Type == projection.TypeEventOrphaned {
			t.Fatalf("unexpected EventOrphaned: %+v", e)
		}
	}
	if !gotTriage {
		t.Fatal("expected TriageDecided in log")
	}
}

func TestTriageEndToEndHealthyOnArchai98(t *testing.T) {
	var buf bytes.Buffer
	prev := handler.SetPrinterOutput(&buf)
	t.Cleanup(func() { handler.SetPrinterOutput(prev) })

	fixDir := filepath.Join("..", "..", "pkg", "handler", "testdata")
	stub := &stubRunner{files: map[string][]byte{
		"/comments": loadFile(t, filepath.Join(fixDir, "archai_98_comments.json")),
		"/timeline": loadFile(t, filepath.Join(fixDir, "archai_98_timeline.json")),
	}}
	prevRunner := handler.SetDefaultGhRunner(stub)
	t.Cleanup(func() { handler.SetDefaultGhRunner(prevRunner) })

	reg := handler.BuiltinRegistry()
	cfg, _ := config.Parse([]byte(triageYAML), reg.Types())
	b, _ := Build(cfg, reg)
	res, err := Run(context.Background(), b, "archai#98")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(buf.String(), "HEALTHY") {
		t.Fatalf("printer output = %q, want HEALTHY", buf.String())
	}
	if !res.State.Handled {
		t.Fatal("expected RequestHandled")
	}
	for _, e := range res.Events {
		if e.Type == projection.TypeEventOrphaned {
			t.Fatalf("unexpected EventOrphaned: %+v", e)
		}
	}
}

func TestTriageEndToEnd404UnhandledOnArchai9999(t *testing.T) {
	stub := &stubRunner{fail: true}
	prevRunner := handler.SetDefaultGhRunner(stub)
	t.Cleanup(func() { handler.SetDefaultGhRunner(prevRunner) })

	reg := handler.BuiltinRegistry()
	cfg, _ := config.Parse([]byte(triageYAML), reg.Types())
	b, _ := Build(cfg, reg)
	res, err := Run(context.Background(), b, "archai#9999")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.State.Handled {
		t.Fatal("expected unhandled for 404")
	}
	if !res.State.Unhandled {
		t.Fatal("expected RequestUnhandled")
	}
	gotFailed := false
	for _, e := range res.Events {
		if e.Type == projection.TypeGhQueryFailed {
			gotFailed = true
		}
	}
	if !gotFailed {
		t.Fatal("expected GhQueryFailed event")
	}
}
