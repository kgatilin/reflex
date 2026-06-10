package handler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/projection"
)

// fakeRunner records the last invocation and returns canned data.
type fakeRunner struct {
	stdout []byte
	stderr []byte
	err    error
	gotCmd string
	gotArg []string
}

func (f *fakeRunner) Run(name string, args ...string) ([]byte, []byte, error) {
	f.gotCmd = name
	f.gotArg = args
	return f.stdout, f.stderr, f.err
}

func TestGhQuerySuccessEmitsResultNonTerminal(t *testing.T) {
	fr := &fakeRunner{stdout: []byte(`[{"id":1}]`)}
	prev := SetDefaultGhRunner(fr)
	t.Cleanup(func() { SetDefaultGhRunner(prev) })

	sub, err := newGhQuery(config.HandlerConfig{
		Name: "fetch", Type: "gh_query", On: projection.TypeTargetParsed,
		Config: map[string]any{"path": "comments"},
	})
	if err != nil {
		t.Fatalf("newGhQuery: %v", err)
	}
	ev := event.Event{
		Type:    projection.TypeTargetParsed,
		Payload: jsonRaw(map[string]any{"owner": "kgatilin", "repo": "archai", "number": 114}),
	}
	out, err := sub.React(context.Background(), ev, nil)
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 event, got %d", len(out))
	}
	if out[0].Type != projection.TypeGhQueryResult {
		t.Fatalf("type = %s", out[0].Type)
	}
	if out[0].Terminal {
		t.Fatal("GhQueryResult must not be terminal")
	}
	if fr.gotCmd != "gh" {
		t.Fatalf("cmd = %s", fr.gotCmd)
	}
	wantSuffix := "repos/kgatilin/archai/issues/114/comments"
	joined := strings.Join(fr.gotArg, " ")
	if !strings.Contains(joined, wantSuffix) {
		t.Fatalf("args %v missing %q", fr.gotArg, wantSuffix)
	}
	var p struct {
		Path string `json:"path"`
	}
	_ = out[0].PayloadAs(&p)
	if p.Path != "comments" {
		t.Fatalf("path = %q", p.Path)
	}
}

func TestGhQueryFailureEmitsFailedTerminal(t *testing.T) {
	fr := &fakeRunner{
		stderr: []byte("gh: Not Found (HTTP 404)"),
		err:    errors.New("exit 1"),
	}
	prev := SetDefaultGhRunner(fr)
	t.Cleanup(func() { SetDefaultGhRunner(prev) })

	sub, _ := newGhQuery(config.HandlerConfig{
		Name: "fetch", Type: "gh_query", On: projection.TypeTargetParsed,
		Config: map[string]any{"path": "timeline"},
	})
	ev := event.Event{
		Type:    projection.TypeTargetParsed,
		Payload: jsonRaw(map[string]any{"owner": "kgatilin", "repo": "archai", "number": 9999}),
	}
	out, err := sub.React(context.Background(), ev, nil)
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	if len(out) != 1 || out[0].Type != projection.TypeGhQueryFailed {
		t.Fatalf("got %+v", out)
	}
	if !out[0].Terminal {
		t.Fatal("GhQueryFailed must be terminal")
	}
	var p struct {
		Stderr string `json:"stderr"`
	}
	_ = out[0].PayloadAs(&p)
	if !strings.Contains(p.Stderr, "404") {
		t.Fatalf("stderr = %q", p.Stderr)
	}
}

func TestGhQueryRequiresPath(t *testing.T) {
	_, err := newGhQuery(config.HandlerConfig{
		Name: "fetch", Type: "gh_query", On: projection.TypeTargetParsed,
	})
	if err == nil {
		t.Fatal("expected error when path missing")
	}
}
