package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/graph"
	"github.com/kgatilin/reflex/pkg/handler"
)

// validateConfigUseCase is the cobra-free core of the validate subcommand;
// it returns (output, exitCode). Keeping it in the test file is fine for
// now — if we later promote it to a runnable helper we can move it.
//
// Why no exec-the-binary subprocess test: the cobra command calls os.Exit
// on cycle errors, which is awkward to assert against. We exercise the
// same logic graph.Build runs and assert exit semantics from the typed
// error.
func validateConfigUseCase(configPath string) (string, int) {
	reg := handler.BuiltinRegistry()
	cfg, err := config.Load(configPath, reg.Types())
	if err != nil {
		return err.Error(), 1
	}
	g, err := graph.Build(cfg, reg)
	if err != nil {
		return err.Error(), 1
	}
	var buf bytes.Buffer
	buf.WriteString("config valid: ")
	buf.WriteString(itoa(len(g.Nodes)))
	buf.WriteString(" handlers, ")
	buf.WriteString(itoa(len(g.Edges)))
	buf.WriteString(" edges, ")
	buf.WriteString(itoa(len(g.DeclaredLoops)))
	buf.WriteString(" declared loops\n")
	return buf.String(), 0
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func examplePath(t *testing.T, name string) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Walk up to repo root (where examples/ lives).
	for i := 0; i < 5; i++ {
		p := filepath.Join(cwd, "examples", name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
		cwd = filepath.Dir(cwd)
	}
	t.Fatalf("can't find examples/%s", name)
	return ""
}

func TestValidateBadLoopExitsNonZero(t *testing.T) {
	out, code := validateConfigUseCase(examplePath(t, "bad_loop.yaml"))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; output = %q", code, out)
	}
	if !strings.Contains(out, "cycle detected") {
		t.Fatalf("missing cycle-detected message: %q", out)
	}
}

func TestValidateCycleExitsNonZero(t *testing.T) {
	out, code := validateConfigUseCase(examplePath(t, "cycle.yaml"))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; output = %q", code, out)
	}
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") || !strings.Contains(out, "gamma") {
		t.Fatalf("cycle should mention all three nodes: %q", out)
	}
}

func TestValidateLoopAcceptedExitsZero(t *testing.T) {
	out, code := validateConfigUseCase(examplePath(t, "loop.yaml"))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output = %q", code, out)
	}
	if !strings.Contains(out, "1 declared loops") {
		t.Fatalf("expected '1 declared loops' in output: %q", out)
	}
}
