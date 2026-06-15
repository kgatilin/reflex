// Package verifier is the verification body (doc 29 §4b, CONCEPT §3/§9:
// "completion is a verified state, never an LLM claim and never mere
// quiescence"). It subscribes to the brain's claim.complete and re-runs an
// agreed check command DETERMINISTICALLY. Green (exit 0) ⇒ it writes the
// verified terminal status (state.updated.status{done}); red ⇒ it emits
// verify.failed back into the loop with the failing output, so the brain gets
// another turn. "Done" is a value this body writes, not one the model claims.
//
// The verifier runs the same kind of command the pytest hand runs, but the roles
// differ: the brain's tool.py.test call is advisory (the model deciding what to
// run); the verifier's run is the authoritative, independent judge of done. The
// command it runs is the verifier target (doc 29 §8 open question), supplied as
// config per run.
//
// Config (BodyConfig):
//
//	command:     argv of the check to run (required, e.g. ["pytest","-q"]).
//	dir:         working directory for the command (optional).
//	status_kind: the verified state-transition kind (default state.updated.status).
//	pass:        the status value written on green (default "done").
//	fail_emit:   the kind emitted on red (default "verify.failed").
//	max_output:  bytes of combined output to keep on the failed event (default 8192).
package verifier

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"

	"github.com/kgatilin/reflex/engine"
)

// Config is the verifier body's descriptor.
type Config struct {
	Command    []string `json:"command"`
	Dir        string   `json:"dir,omitempty"`
	StatusKind string   `json:"status_kind,omitempty"`
	Pass       string   `json:"pass,omitempty"`
	FailEmit   string   `json:"fail_emit,omitempty"`
	MaxOutput  int      `json:"max_output,omitempty"`
}

const (
	defaultStatusKind = "state.updated.status"
	defaultPass       = "done"
	defaultFailEmit   = "verify.failed"
	defaultMaxOutput  = 8192
)

// Factory is the nodes.Factory for body kind "verifier". Register it with
// nodes.Register("verifier", verifier.Factory).
func Factory(s engine.Subscriber) (engine.Reaction, error) {
	config := s.BodyConfig
	var cfg Config
	if len(config) > 0 {
		if err := json.Unmarshal(config, &cfg); err != nil {
			return nil, err
		}
	}
	if len(cfg.Command) == 0 {
		return nil, fmt.Errorf("verifier: no command in config")
	}
	if cfg.StatusKind == "" {
		cfg.StatusKind = defaultStatusKind
	}
	if cfg.Pass == "" {
		cfg.Pass = defaultPass
	}
	if cfg.FailEmit == "" {
		cfg.FailEmit = defaultFailEmit
	}
	if cfg.MaxOutput <= 0 {
		cfg.MaxOutput = defaultMaxOutput
	}

	return engine.ReactionFunc(func(ctx context.Context, _ engine.Event, _ engine.Views) ([]engine.Emit, error) {
		cmd := exec.CommandContext(ctx, cfg.Command[0], cfg.Command[1:]...)
		if cfg.Dir != "" {
			cmd.Dir = cfg.Dir
		}
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()

		if err == nil {
			// Verified green: write the terminal status. This is the ONLY writer of
			// the verified done — completion as a value, not a claim (CONCEPT §3/§9).
			payload, _ := json.Marshal(map[string]string{"status": cfg.Pass})
			return []engine.Emit{{Kind: cfg.StatusKind, Payload: payload}}, nil
		}

		// Red: a clean non-zero exit (the check failed) and a launch failure (bad
		// command) both feed verify.failed back into the loop — a failed check is an
		// event, not control flow (G3). The brain gets another turn with the output.
		exitCode := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode()
		}
		payload, _ := json.Marshal(struct {
			Error     string `json:"error"`
			ExitCode  int    `json:"exit_code"`
			Output    string `json:"output"`
			Truncated bool   `json:"truncated,omitempty"`
		}{
			Error:     err.Error(),
			ExitCode:  exitCode,
			Output:    truncate(out.String(), cfg.MaxOutput),
			Truncated: out.Len() > cfg.MaxOutput,
		})
		return []engine.Emit{{Kind: cfg.FailEmit, Payload: payload}}, nil
	}), nil
}

// truncate keeps the LAST n bytes of s (test failures surface at the tail).
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
