// The gotool plugin: doc 14's deterministic single-purpose validators —
// tool.go.build / tool.go.test / tool.go.vet — as fixed-argv wrappers around
// the go toolchain. This is the bash replacement: no shell ever parses the
// arguments, the package pattern is validated against a strict relative-path
// grammar, and the only thing each tool can do is its one job rooted at
// --root.
//
// A compile/test/vet failure is a RESULT (ok=false + output) — the verdict
// the brain reads and reacts to. tool.go.*.failed is reserved for the
// wrapper itself being unusable (bad params, go binary missing).
package main

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const outputCap = 16 << 10 // bytes of command output kept per side of the cut

type goTools struct {
	root    string
	timeout time.Duration
}

// pkgPattern is the only accepted package-argument grammar: a relative
// import pattern like ./... or ./pkg/handler or ./pkg/... — never an
// absolute path, never something that could be parsed as a flag.
var pkgPattern = regexp.MustCompile(`^\./[a-zA-Z0-9_./\-]*$`)

func validPackage(pkg string) (string, error) {
	if pkg == "" {
		return "./...", nil
	}
	// After stripping a trailing "..." wildcard, no ".." may remain — that
	// would be a path escape, not an import pattern.
	if !pkgPattern.MatchString(pkg) || strings.Contains(strings.TrimSuffix(pkg, "..."), "..") {
		return "", fmt.Errorf("package %q must be a relative pattern like ./... or ./pkg/name", pkg)
	}
	return pkg, nil
}

// runResult is the shared verdict payload of all three tools.
type runResult struct {
	OK         bool   `json:"ok"`
	Output     string `json:"output"`
	Truncated  bool   `json:"truncated"`
	DurationMS int64  `json:"duration_ms"`
}

type buildParams struct {
	Package string `json:"package"`
}

type testParams struct {
	Package string `json:"package"`
	Run     string `json:"run"` // optional -run regex
}

type vetParams struct {
	Package string `json:"package"`
}

func (g *goTools) build(p buildParams) (runResult, error) {
	pkg, err := validPackage(p.Package)
	if err != nil {
		return runResult{}, err
	}
	return g.run("build", pkg, nil)
}

func (g *goTools) test(p testParams) (runResult, error) {
	pkg, err := validPackage(p.Package)
	if err != nil {
		return runResult{}, err
	}
	var extra []string
	if p.Run != "" {
		if strings.HasPrefix(p.Run, "-") {
			return runResult{}, fmt.Errorf("run %q must be a test-name regex, not a flag", p.Run)
		}
		extra = []string{"-run", p.Run}
	}
	return g.run("test", pkg, extra)
}

func (g *goTools) vet(p vetParams) (runResult, error) {
	pkg, err := validPackage(p.Package)
	if err != nil {
		return runResult{}, err
	}
	return g.run("vet", pkg, nil)
}

// run executes one go subcommand with a fixed argv, rooted at g.root, under
// the plugin's timeout. The exit status maps to ok; combined output is
// capped head+tail so neither the first compile error nor the final test
// summary is lost.
func (g *goTools) run(sub, pkg string, extra []string) (runResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), g.timeout)
	defer cancel()

	args := append([]string{sub}, extra...)
	args = append(args, pkg)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = g.root

	start := time.Now()
	out, err := cmd.CombinedOutput()
	dur := time.Since(start)

	res := runResult{DurationMS: dur.Milliseconds()}
	res.Output, res.Truncated = capOutput(string(out))
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		res.OK = false
		res.Output = fmt.Sprintf("go %s timed out after %s\n%s", sub, g.timeout, res.Output)
	case err == nil:
		res.OK = true
	default:
		if _, isExit := err.(*exec.ExitError); !isExit {
			// Not a command verdict — the wrapper itself failed to run go.
			return runResult{}, fmt.Errorf("go %s: %w", sub, err)
		}
		res.OK = false
	}
	if res.OK && strings.TrimSpace(res.Output) == "" {
		res.Output = "ok (no output)"
	}
	return res, nil
}

// capOutput keeps the head and tail of oversized output with an explicit
// elision marker — never a silent cut.
func capOutput(s string) (string, bool) {
	if len(s) <= 2*outputCap {
		return s, false
	}
	dropped := len(s) - 2*outputCap
	return s[:outputCap] + fmt.Sprintf("\n...[%d bytes truncated]...\n", dropped) + s[len(s)-outputCap:], true
}
