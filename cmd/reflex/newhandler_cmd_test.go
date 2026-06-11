package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/handler"
)

func TestScaffoldRejectsInvalidName(t *testing.T) {
	cases := []string{"", "1bad", "Bad", "with_space ", "with.dot", "with/slash"}
	for _, n := range cases {
		opts := &scaffoldOptions{Name: n, Consumes: "X", Language: "yaml"}
		if _, err := runScaffold(opts); err == nil {
			t.Errorf("name %q: expected error", n)
		}
	}
}

func TestScaffoldRejectsInvalidEventType(t *testing.T) {
	opts := &scaffoldOptions{
		Name:     "ok",
		Consumes: "Bad Event",
		Language: "yaml",
	}
	if _, err := runScaffold(opts); err == nil {
		t.Fatal("expected invalid consumes to be rejected")
	}
	opts = &scaffoldOptions{
		Name:     "ok",
		Consumes: "Good",
		Emits:    []string{"alsoOK", "bad event"},
		Language: "yaml",
	}
	if _, err := runScaffold(opts); err == nil {
		t.Fatal("expected invalid emits to be rejected")
	}
}

func TestScaffoldRequiresConsumes(t *testing.T) {
	opts := &scaffoldOptions{Name: "ok", Language: "yaml"}
	if _, err := runScaffold(opts); err == nil {
		t.Fatal("expected --consumes-required error")
	}
}

func TestScaffoldYAMLStdoutBlock(t *testing.T) {
	opts := &scaffoldOptions{
		Name:     "my-classifier",
		Consumes: "Classification",
		Emits:    []string{"ClassificationResult", "RequestHandled"},
		Terminal: []string{"RequestHandled"},
		Scope:    "tools.classifiers",
		Language: "yaml",
	}
	out, err := runScaffold(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"name: my-classifier",
		"on: Classification",
		"emits: [ClassificationResult, RequestHandled]",
		"scope: tools.classifiers",
		"NOTE: terminal emits — RequestHandled",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestScaffoldYAMLAppendToFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "scaffold-test.yaml")
	seed := `handlers:
  - name: existing
    type: echo
    on: RequestReceived
`
	if err := os.WriteFile(configPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := &scaffoldOptions{
		Name:       "new-one",
		Consumes:   "Foo",
		Emits:      []string{"Bar"},
		ConfigPath: configPath,
		Language:   "yaml",
	}
	out, err := runScaffold(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "appended handler") {
		t.Errorf("expected append confirmation, got %q", out)
	}
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "name: new-one") {
		t.Fatalf("appended file missing new handler:\n%s", string(got))
	}
	if !strings.Contains(string(got), "name: existing") {
		t.Fatalf("original handler was lost:\n%s", string(got))
	}
}

func TestScaffoldYAMLRefusesDuplicateName(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "scaffold-test.yaml")
	seed := `handlers:
  - name: dupe
    type: echo
    on: RequestReceived
`
	if err := os.WriteFile(configPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := &scaffoldOptions{
		Name:       "dupe",
		Consumes:   "Foo",
		ConfigPath: configPath,
		Language:   "yaml",
	}
	_, err := runScaffold(opts)
	if err == nil {
		t.Fatal("expected duplicate name to be rejected")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error doesn't mention dupe: %v", err)
	}
}

func TestScaffoldYAMLRefusesWithoutHandlersKey(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "scaffold-test.yaml")
	if err := os.WriteFile(configPath, []byte("settings:\n  max_steps: 64\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := &scaffoldOptions{
		Name:       "lonely",
		Consumes:   "Foo",
		ConfigPath: configPath,
		Language:   "yaml",
	}
	_, err := runScaffold(opts)
	if err == nil {
		t.Fatal("expected refusal when no handlers: key present")
	}
}

func TestScaffoldYAMLAppendedConfigIsLoadable(t *testing.T) {
	// Generate a complete config from scratch (using a known handler
	// type) and confirm pkg/config + pkg/graph happily ingest it.
	dir := t.TempDir()
	configPath := filepath.Join(dir, "loadable.yaml")
	seed := `handlers:
  - name: seed
    type: echo
    on: RequestReceived
    config: { emit: ParsedOK }
`
	if err := os.WriteFile(configPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := &scaffoldOptions{
		Name:       "follower",
		Consumes:   "ParsedOK",
		Emits:      []string{"RequestHandled"},
		Terminal:   []string{"RequestHandled"},
		ConfigPath: configPath,
		Language:   "yaml",
	}
	if _, err := runScaffold(opts); err != nil {
		t.Fatal(err)
	}
	// Patch TODO_handler_type → terminator so the config parses against
	// the real registry. (Real users do this by hand; the test asserts
	// the surrounding YAML structure is valid.)
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.Replace(string(raw), "TODO_handler_type", "terminator", 1)
	if err := os.WriteFile(configPath, []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := handler.BuiltinRegistry()
	if _, err := config.Load(configPath, reg.Types()); err != nil {
		t.Fatalf("config.Load: %v\n--- yaml ---\n%s", err, patched)
	}
}

func TestScaffoldGoWritesFileAndVetsClean(t *testing.T) {
	// Find the repo root by walking up from the test cwd until we hit
	// go.mod. We need the generated file to compile against the local
	// pkg/sdk import path.
	repoRoot := findRepoRoot(t)

	dir := t.TempDir()
	outDir := filepath.Join(dir, "cmd", "scaffolded-handler")
	opts := &scaffoldOptions{
		Name:      "scaffolded-handler",
		Consumes:  "RequestReceived",
		Emits:     []string{"Response", "Done"},
		Terminal:  []string{"Done"},
		Scope:     "test.scaffold",
		Language:  "go",
		OutputDir: outDir,
	}
	if _, err := runScaffold(opts); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outDir, "main.go")
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("scaffolded file not written: %v", err)
	}
	for _, want := range []string{
		"package main",
		`sdk.Consumes("RequestReceived")`,
		`sdk.Emits("Response")`,
		`sdk.Emits("Done")`,
		`sdk.Terminal("Done")`,
		`sdk.WithScope("test.scaffold")`,
		"OnEvent(func(ctx sdk.Ctx, ev sdk.Event) error",
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("scaffolded file missing %q\n--- file ---\n%s", want, body)
		}
	}

	// Now `go vet` it inside the live repo module. Drop the file into
	// the repo's cmd/ tree so it resolves the module-relative import.
	vetTarget := filepath.Join(repoRoot, "cmd", "scaffolded-handler-test")
	if err := os.MkdirAll(vetTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(vetTarget)
	if err := os.WriteFile(filepath.Join(vetTarget, "main.go"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "vet", "./cmd/scaffolded-handler-test/...")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "GOFLAGS=")
	combined, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go vet on scaffolded file failed: %v\n%s", err, combined)
	}
}

func TestScaffoldGoRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "cmd", "exists")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := &scaffoldOptions{
		Name:      "exists",
		Consumes:  "Foo",
		Language:  "go",
		OutputDir: outDir,
	}
	_, err := runScaffold(opts)
	if err == nil {
		t.Fatal("expected refusal when main.go exists")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error doesn't mention pre-existing file: %v", err)
	}
}

func TestScaffoldUnknownLanguage(t *testing.T) {
	opts := &scaffoldOptions{
		Name:     "ok",
		Consumes: "Foo",
		Language: "rust",
	}
	_, err := runScaffold(opts)
	if err == nil {
		t.Fatal("expected unknown-language error")
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(filepath.Join(cwd, "go.mod")); err == nil {
			return cwd
		}
		cwd = filepath.Dir(cwd)
	}
	t.Fatal("could not find go.mod walking up from test cwd")
	return ""
}
