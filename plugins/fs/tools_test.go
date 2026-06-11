package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestTools(t *testing.T) (*fsTools, string) {
	t.Helper()
	root := t.TempDir()
	tools, err := newFSTools(root)
	if err != nil {
		t.Fatalf("newFSTools: %v", err)
	}
	return tools, root
}

func writeFixture(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveClampsAndRejects(t *testing.T) {
	tools, _ := newTestTools(t)
	if _, _, err := tools.resolve("/etc/passwd"); err == nil {
		t.Error("absolute path must be rejected")
	}
	if _, _, err := tools.resolve(""); err == nil {
		t.Error("empty path must be rejected")
	}
	// A ..-escape is clamped back under root, not let out.
	abs, rel, err := tools.resolve("../../outside.txt")
	if err != nil {
		t.Fatalf("clamped path errored: %v", err)
	}
	if rel != "outside.txt" || !strings.HasPrefix(abs, tools.root) {
		t.Errorf("clamp gave abs=%q rel=%q", abs, rel)
	}
}

func TestReadWindow(t *testing.T) {
	tools, root := newTestTools(t)
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, "line")
	}
	writeFixture(t, root, "pkg/foo/bar.go", strings.Join(lines, "\n")+"\n")

	res, err := tools.read(readParams{Path: "pkg/foo/bar.go", Offset: 3, Limit: 4})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if res.Lines != (lineRange{From: 3, To: 6, Total: 10}) {
		t.Errorf("lines = %+v", res.Lines)
	}
	if !res.Truncated {
		t.Error("window ends before EOF — truncated must be true")
	}
	if !strings.Contains(res.Content, "     3\tline") {
		t.Errorf("content not line-numbered:\n%s", res.Content)
	}
	if res.SHA == "" || len(res.SHA) != 12 {
		t.Errorf("sha = %q", res.SHA)
	}

	if _, err := tools.read(readParams{Path: "pkg/foo/bar.go", Offset: 99}); err == nil {
		t.Error("offset beyond EOF must fail")
	}
	if _, err := tools.read(readParams{Path: "missing.go"}); err == nil {
		t.Error("missing file must fail")
	}
}

func TestEditGuards(t *testing.T) {
	tools, root := newTestTools(t)
	writeFixture(t, root, "a.go", "func Parse() {\n\treturn nil\n}\n")

	// No prior read.
	if _, err := tools.edit(editParams{Path: "a.go", Old: "return nil", New: "return err"}); err == nil || !strings.Contains(err.Error(), "no prior read") {
		t.Fatalf("want no-prior-read failure, got %v", err)
	}

	if _, err := tools.read(readParams{Path: "a.go"}); err != nil {
		t.Fatal(err)
	}

	// Stale read: file changes behind the model's back.
	writeFixture(t, root, "a.go", "func Parse() {\n\treturn nil // changed\n}\n")
	if _, err := tools.edit(editParams{Path: "a.go", Old: "return nil", New: "return err"}); err == nil || !strings.Contains(err.Error(), "stale read") {
		t.Fatalf("want stale-read failure, got %v", err)
	}

	// Re-read heals it.
	if _, err := tools.read(readParams{Path: "a.go"}); err != nil {
		t.Fatal(err)
	}
	res, err := tools.edit(editParams{Path: "a.go", Old: "return nil // changed", New: "return errEmpty"})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if res.Replaced != 1 {
		t.Errorf("replaced = %d", res.Replaced)
	}
	got, _ := os.ReadFile(filepath.Join(root, "a.go"))
	if !strings.Contains(string(got), "errEmpty") {
		t.Errorf("edit not applied: %s", got)
	}

	// Chained edit without re-read works — the edit result updated seen.
	if _, err := tools.edit(editParams{Path: "a.go", Old: "errEmpty", New: "errBlank"}); err != nil {
		t.Errorf("read→edit→edit chain must not force a re-read: %v", err)
	}
}

func TestEditUniqueness(t *testing.T) {
	tools, root := newTestTools(t)
	writeFixture(t, root, "b.go", "x := 1\nx := 1\n")
	if _, err := tools.read(readParams{Path: "b.go"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.edit(editParams{Path: "b.go", Old: "x := 1", New: "y := 1"}); err == nil || !strings.Contains(err.Error(), "2 times") {
		t.Fatalf("non-unique old must fail, got %v", err)
	}
	res, err := tools.edit(editParams{Path: "b.go", Old: "x := 1", New: "y := 1", ReplaceAll: true})
	if err != nil {
		t.Fatalf("replace_all: %v", err)
	}
	if res.Replaced != 2 {
		t.Errorf("replaced = %d", res.Replaced)
	}
	if _, err := tools.edit(editParams{Path: "b.go", Old: "absent", New: "z"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("absent old must fail, got %v", err)
	}
}

func TestWriteCreateAndOverwriteGuard(t *testing.T) {
	tools, root := newTestTools(t)

	// Create (with directory creation).
	res, err := tools.write(writeParams{Path: "new/dir/file.go", Content: "package dir\n"})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if res.Path != "new/dir/file.go" || res.SHA == "" {
		t.Errorf("result = %+v", res)
	}

	// Overwrite of a file the context has seen (we just wrote it) is allowed.
	if _, err := tools.write(writeParams{Path: "new/dir/file.go", Content: "package dir // v2\n"}); err != nil {
		t.Errorf("overwrite after write: %v", err)
	}

	// Overwrite of an existing never-read file is refused.
	writeFixture(t, root, "stranger.go", "package x\n")
	if _, err := tools.write(writeParams{Path: "stranger.go", Content: "clobber"}); err == nil || !strings.Contains(err.Error(), "never read") {
		t.Fatalf("want never-read refusal, got %v", err)
	}
}

func TestSearch(t *testing.T) {
	tools, root := newTestTools(t)
	writeFixture(t, root, "pkg/foo/bar.go", "package foo\n\nfunc Parse(s string) error {\n\treturn nil\n}\n")
	writeFixture(t, root, "pkg/foo/baz.go", "package foo\n\nfunc Render() {}\n")
	writeFixture(t, root, ".git/objects/blob", "func Parse should not match\x00binary")
	writeFixture(t, root, "vendor.bin", "func Parse\x00")

	res, err := tools.search(searchParams{Query: "func Parse"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Matches) != 1 {
		t.Fatalf("matches = %+v", res.Matches)
	}
	m := res.Matches[0]
	if m.Path != "pkg/foo/bar.go" || m.Line != 3 || !strings.Contains(m.Text, "func Parse") {
		t.Errorf("match = %+v", m)
	}

	// Glob narrows by base name; regex mode compiles.
	res, err = tools.search(searchParams{Query: "func", Glob: "baz.go"})
	if err != nil || len(res.Matches) != 1 || res.Matches[0].Path != "pkg/foo/baz.go" {
		t.Errorf("glob search = %+v (err %v)", res.Matches, err)
	}
	res, err = tools.search(searchParams{Query: `func (Parse|Render)`, Regex: true})
	if err != nil || len(res.Matches) != 2 {
		t.Errorf("regex search = %+v (err %v)", res.Matches, err)
	}
	if _, err := tools.search(searchParams{Query: "(", Regex: true}); err == nil {
		t.Error("bad regex must fail")
	}
	if _, err := tools.search(searchParams{}); err == nil {
		t.Error("empty query must fail")
	}

	// Max caps with the truncated flag, no silent cut.
	res, err = tools.search(searchParams{Query: "package", Max: 1})
	if err != nil || len(res.Matches) != 1 || !res.Truncated {
		t.Errorf("max-capped search = %+v truncated=%v (err %v)", res.Matches, res.Truncated, err)
	}
}

func TestSearchMarksNothingSeen(t *testing.T) {
	tools, root := newTestTools(t)
	writeFixture(t, root, "c.go", "target line\n")
	if _, err := tools.search(searchParams{Query: "target"}); err != nil {
		t.Fatal(err)
	}
	// A search hit is not a read — editing still requires a real read.
	if _, err := tools.edit(editParams{Path: "c.go", Old: "target", New: "x"}); err == nil || !strings.Contains(err.Error(), "no prior read") {
		t.Fatalf("search must not satisfy the read guard, got %v", err)
	}
}
