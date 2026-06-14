package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kgatilin/reflex/pkg/plugin"
)

func mustTools(t *testing.T) *fsTools {
	t.Helper()
	f, err := newFSTools(t.TempDir())
	if err != nil {
		t.Fatalf("newFSTools: %v", err)
	}
	return f
}

// TestFS_WriteReadEdit exercises the happy path through the handler: write a
// file, read it back (line-numbered), then edit it under the read-before-edit
// guard.
func TestFS_WriteReadEdit(t *testing.T) {
	f := mustTools(t)

	if k := emitKind(t, f, "tool.fs.write.call", `{"path":"a.txt","content":"alpha\nbeta\n"}`); k != "tool.fs.write.result" {
		t.Fatalf("write emitted %q, want tool.fs.write.result", k)
	}
	if k := emitKind(t, f, "tool.fs.read.call", `{"path":"a.txt"}`); k != "tool.fs.read.result" {
		t.Fatalf("read emitted %q, want tool.fs.read.result", k)
	}
	// edit needs a prior read (write counts) and exact unique old.
	if k := emitKind(t, f, "tool.fs.edit.call", `{"path":"a.txt","old":"beta","new":"gamma"}`); k != "tool.fs.edit.result" {
		t.Fatalf("edit emitted %q, want tool.fs.edit.result", k)
	}
	got, _ := os.ReadFile(filepath.Join(f.root, "a.txt"))
	if string(got) != "alpha\ngamma\n" {
		t.Errorf("file = %q, want alpha\\ngamma\\n", got)
	}
}

// TestFS_EditRequiresPriorRead proves the read-before-edit guard: editing a file
// that exists on disk but was never read through the plugin fails (emits failed).
func TestFS_EditRequiresPriorRead(t *testing.T) {
	f := mustTools(t)
	if err := os.WriteFile(filepath.Join(f.root, "x.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if k := emitKind(t, f, "tool.fs.edit.call", `{"path":"x.txt","old":"one","new":"two"}`); k != "tool.fs.edit.failed" {
		t.Errorf("edit without prior read emitted %q, want tool.fs.edit.failed", k)
	}
}

// TestFS_PathConfinement: absolute paths are rejected outright, and a `..`
// escape is clamped under the root rather than escaping it.
func TestFS_PathConfinement(t *testing.T) {
	f := mustTools(t)
	if _, _, err := f.resolve("/etc/passwd"); err == nil {
		t.Error("absolute path was not rejected")
	}
	abs, rel, err := f.resolve("../../../../etc/passwd")
	if err != nil {
		t.Fatalf("clamped path errored: %v", err)
	}
	if rel != "etc/passwd" {
		t.Errorf("rel = %q, want etc/passwd (clamped)", rel)
	}
	if !strings.HasPrefix(abs, f.root) {
		t.Errorf("abs %q escaped root %q", abs, f.root)
	}
}

// TestFS_Search finds matches across files, sorted by path then line.
func TestFS_Search(t *testing.T) {
	f := mustTools(t)
	for _, n := range []string{"b.txt", "a.txt"} {
		if err := os.WriteFile(filepath.Join(f.root, n), []byte("needle here\nother\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := f.search(searchParams{Query: "needle"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Matches) != 2 || res.Matches[0].Path != "a.txt" {
		t.Fatalf("matches = %+v; want 2, a.txt first (sorted)", res.Matches)
	}
}

// TestFSHandler_UnknownKind returns a Go error (a kind the node should never
// have delivered), distinct from an op failure (which emits .failed).
func TestFSHandler_UnknownKind(t *testing.T) {
	f := mustTools(t)
	if _, err := f.handle(plugin.Event{Kind: "tool.fs.nope.call"}); err == nil {
		t.Error("unknown kind returned nil error")
	}
}

func TestBuildFS_BadRoot(t *testing.T) {
	if _, _, err := buildFS(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Error("buildFS with a missing root returned nil error")
	}
}

// emitKind runs one call through the handler and returns the single emitted kind.
func emitKind(t *testing.T, f *fsTools, kind, payload string) string {
	t.Helper()
	emits, err := f.handle(plugin.Event{Kind: kind, Payload: json.RawMessage(payload)})
	if err != nil {
		t.Fatalf("handle %s: %v", kind, err)
	}
	if len(emits) != 1 {
		t.Fatalf("handle %s emitted %d events, want 1: %+v", kind, len(emits), emits)
	}
	return emits[0].Kind
}
