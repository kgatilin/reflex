package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestGoTools(t *testing.T) (*goTools, string) {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, root, "go.mod", "module example.com/fixture\n\ngo 1.24\n")
	mustWrite(t, root, "lib.go", "package fixture\n\nfunc Answer() int { return 42 }\n")
	mustWrite(t, root, "lib_test.go", `package fixture

import "testing"

func TestAnswer(t *testing.T) {
	if Answer() != 42 {
		t.Fatal("wrong answer")
	}
}
`)
	return &goTools{root: root, timeout: 2 * time.Minute}, root
}

func mustWrite(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestValidPackage(t *testing.T) {
	for in, want := range map[string]string{
		"":          "./...",
		"./...":     "./...",
		"./pkg/foo": "./pkg/foo",
		"./pkg/...": "./pkg/...",
	} {
		got, err := validPackage(in)
		if err != nil || got != want {
			t.Errorf("validPackage(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"/abs/path", "-gcflags=evil", "pkg/foo", "./../escape", "./../...", "./a b"} {
		if _, err := validPackage(in); err == nil {
			t.Errorf("validPackage(%q): want error", in)
		}
	}
}

func TestBuildOKAndFailure(t *testing.T) {
	tools, root := newTestGoTools(t)

	res, err := tools.build(buildParams{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !res.OK {
		t.Fatalf("build not ok: %s", res.Output)
	}
	if res.DurationMS < 0 {
		t.Error("duration missing")
	}

	mustWrite(t, root, "broken.go", "package fixture\n\nfunc Broken() int { return }\n")
	res, err = tools.build(buildParams{})
	if err != nil {
		t.Fatalf("build (broken): %v", err)
	}
	if res.OK {
		t.Fatal("broken build reported ok")
	}
	if !strings.Contains(res.Output, "broken.go") {
		t.Errorf("verdict output missing compiler error: %s", res.Output)
	}
}

func TestTestRunFilterAndFailure(t *testing.T) {
	tools, root := newTestGoTools(t)

	res, err := tools.test(testParams{Run: "TestAnswer"})
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	if !res.OK {
		t.Fatalf("test not ok: %s", res.Output)
	}

	if _, err := tools.test(testParams{Run: "-evilflag"}); err == nil {
		t.Error("flag-shaped run regex must be rejected")
	}

	mustWrite(t, root, "fail_test.go", `package fixture

import "testing"

func TestAlwaysFails(t *testing.T) { t.Fatal("boom") }
`)
	res, err = tools.test(testParams{Run: "TestAlwaysFails"})
	if err != nil {
		t.Fatalf("test (failing): %v", err)
	}
	if res.OK {
		t.Fatal("failing test reported ok")
	}
	if !strings.Contains(res.Output, "boom") {
		t.Errorf("verdict output missing failure: %s", res.Output)
	}
}

func TestVet(t *testing.T) {
	tools, root := newTestGoTools(t)
	res, err := tools.vet(vetParams{})
	if err != nil {
		t.Fatalf("vet: %v", err)
	}
	if !res.OK {
		t.Fatalf("vet not ok: %s", res.Output)
	}

	mustWrite(t, root, "vetbad.go", "package fixture\n\nimport \"fmt\"\n\nfunc Bad() { fmt.Sprintf(\"%d\", \"oops\") }\n")
	res, err = tools.vet(vetParams{})
	if err != nil {
		t.Fatalf("vet (bad): %v", err)
	}
	if res.OK {
		t.Fatal("vet violation reported ok")
	}
}

func TestCapOutput(t *testing.T) {
	small, trunc := capOutput("hello")
	if small != "hello" || trunc {
		t.Errorf("small output mangled: %q %v", small, trunc)
	}
	big := strings.Repeat("a", 3*outputCap)
	capped, trunc := capOutput(big)
	if !trunc || !strings.Contains(capped, "bytes truncated") {
		t.Error("oversized output not visibly truncated")
	}
	if len(capped) > 2*outputCap+100 {
		t.Errorf("capped output still huge: %d", len(capped))
	}
}
