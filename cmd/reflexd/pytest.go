// The pytest plugin (doc 29 Iteration 3b): shells out to pytest as a stdio
// plugin subcommand — tool.py.test.call. It runs pytest in the root dir, then
// classifies the exit: 0 → result{passed:true}, 1 → result{passed:false} (tests
// ran and some failed — a real result the brain reasons about), anything else
// (collection/usage/internal error, or the binary missing) → failed.
package main

import (
	"errors"
	"fmt"
	"os/exec"

	"github.com/kgatilin/reflex/pkg/plugin"
)

// maxPytestOutput caps captured output so one giant run cannot blow the wire.
const maxPytestOutput = 64 * 1024

type pytest struct {
	root string
	// run executes pytest and returns its combined output and exit code; err is
	// non-nil only when the process could not run at all (e.g. binary missing).
	// Injectable so the classification logic is testable without pytest.
	run func(dir string, args []string) (output string, exit int, err error)
}

func buildPytest(root string) (plugin.Spec, plugin.Handler, error) {
	if root == "" {
		root = "."
	}
	t, err := newFSTools(root) // reuse the root validation (must be an existing dir)
	if err != nil {
		return plugin.Spec{}, nil, err
	}
	p := &pytest{root: t.root, run: runPytest}
	return pytestSpec, p.handle, nil
}

var pytestSpec = plugin.Spec{
	Name: "pytest",
	Events: []plugin.EventDecl{
		{Kind: "tool.py.test.call", Role: plugin.RoleIn, Schema: schema(`{
			"type":"object",
			"properties":{
				"args":{"type":"array","items":{"type":"string"},"description":"extra pytest args / target paths or node ids"}
			}
		}`)},
		{Kind: "tool.py.test.result", Role: plugin.RoleOut},
		{Kind: "tool.py.test.failed", Role: plugin.RoleOut},
	},
}

func (p *pytest) handle(ev plugin.Event) ([]plugin.Emit, error) {
	if ev.Kind != "tool.py.test.call" {
		return nil, fmt.Errorf("pytest: unhandled kind %q", ev.Kind)
	}
	var call struct {
		Args []string `json:"args"`
	}
	if err := unmarshal(ev.Payload, &call); err != nil {
		return []plugin.Emit{{Kind: "tool.py.test.failed", Payload: mustJSON(map[string]string{"error": err.Error()})}}, nil
	}

	out, exit, err := p.run(p.root, call.Args)
	out, truncated := capOutput(out)
	switch {
	case err != nil:
		return []plugin.Emit{{Kind: "tool.py.test.failed", Payload: mustJSON(map[string]any{
			"error": fmt.Sprintf("could not run pytest: %v", err), "output": out, "truncated": truncated})}}, nil
	case exit == 0 || exit == 1:
		return []plugin.Emit{{Kind: "tool.py.test.result", Payload: mustJSON(map[string]any{
			"passed": exit == 0, "exit_code": exit, "output": out, "truncated": truncated})}}, nil
	default:
		return []plugin.Emit{{Kind: "tool.py.test.failed", Payload: mustJSON(map[string]any{
			"error":  fmt.Sprintf("pytest exited %d (not a test pass/fail: collection, usage, or internal error)", exit),
			"output": out, "truncated": truncated})}}, nil
	}
}

// runPytest is the production runner.
func runPytest(dir string, args []string) (string, int, error) {
	cmd := exec.Command("pytest", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return string(out), ee.ExitCode(), nil // ran, non-zero exit
		}
		return string(out), -1, err // could not start
	}
	return string(out), 0, nil
}

func capOutput(s string) (string, bool) {
	if len(s) <= maxPytestOutput {
		return s, false
	}
	return s[:maxPytestOutput] + "\n...[truncated]", true
}
