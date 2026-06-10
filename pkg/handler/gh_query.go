package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"sync"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/projection"
)

// CmdRunner abstracts os/exec so unit tests can supply canned responses
// without touching the network. Production wiring uses ExecRunner.
type CmdRunner interface {
	Run(name string, args ...string) (stdout, stderr []byte, err error)
}

// ExecRunner runs the binary on PATH via os/exec.
type ExecRunner struct{}

// Run shells out to name with args. stdout/stderr are returned separately so
// callers can decide what to log.
func (ExecRunner) Run(name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return out, ee.Stderr, err
		}
		return out, nil, err
	}
	return out, nil, nil
}

// defaultRunner is overridable for runtime injection (e.g. tests).
var (
	defaultRunnerMu sync.Mutex
	defaultRunner   CmdRunner = ExecRunner{}
)

// SetDefaultGhRunner swaps the runner used by every gh_query handler created
// after the call. Returns the previous runner so callers can restore it.
// Tests use this to inject a mock without rewriting the YAML factory.
func SetDefaultGhRunner(r CmdRunner) CmdRunner {
	defaultRunnerMu.Lock()
	defer defaultRunnerMu.Unlock()
	prev := defaultRunner
	if r == nil {
		r = ExecRunner{}
	}
	defaultRunner = r
	return prev
}

func currentGhRunner() CmdRunner {
	defaultRunnerMu.Lock()
	defer defaultRunnerMu.Unlock()
	return defaultRunner
}

type ghQueryConfig struct {
	// Path is the sub-path under issues/{number}/. Supported: "comments",
	// "timeline", or any custom GitHub API leaf the user wants.
	Path string `yaml:"path"`
}

// newGhQuery shells out to `gh api repos/{owner}/{repo}/issues/{number}/{path}`
// and emits GhQueryResult (non-terminal) on success, GhQueryFailed (terminal)
// on failure. The handler reads owner/repo/number from the trigger event
// payload — typically a TargetParsed event from parse_target.
func newGhQuery(cfg config.HandlerConfig) (bus.Subscriber, error) {
	var gc ghQueryConfig
	if err := decodeConfig(cfg.Config, &gc); err != nil {
		return nil, fmt.Errorf("gh_query %q: %w", cfg.Name, err)
	}
	if gc.Path == "" {
		return nil, fmt.Errorf("gh_query %q: path is required", cfg.Name)
	}

	name := cfg.Name
	on := cfg.On
	path := gc.Path

	return &genericSub{
		baseSub: baseSub{name: name},
		on:      on,
		run: func(_ context.Context, ev event.Event, _ []event.Event) ([]event.Event, error) {
			var p struct {
				Owner  string `json:"owner"`
				Repo   string `json:"repo"`
				Number int    `json:"number"`
			}
			if err := ev.PayloadAs(&p); err != nil {
				return nil, fmt.Errorf("gh_query %q: decode payload: %w", name, err)
			}
			if p.Owner == "" || p.Repo == "" || p.Number == 0 {
				return nil, fmt.Errorf("gh_query %q: missing owner/repo/number in payload", name)
			}

			runner := currentGhRunner()
			apiPath := "repos/" + p.Owner + "/" + p.Repo + "/issues/" + strconv.Itoa(p.Number) + "/" + path
			// --paginate so timelines / comments lists return every page; gh
			// returns concatenated JSON arrays for paginated endpoints. We
			// trust gh's concatenation since the consumer (triage_rules)
			// folds across the JSON in one pass.
			stdout, stderr, err := runner.Run("gh", "api", "--paginate", apiPath)
			if err != nil {
				failPayload, _ := json.Marshal(map[string]string{
					"path":   path,
					"owner":  p.Owner,
					"repo":   p.Repo,
					"number": strconv.Itoa(p.Number),
					"stderr": string(stderr),
					"error":  err.Error(),
				})
				return []event.Event{{
					Type:     projection.TypeGhQueryFailed,
					Payload:  failPayload,
					Terminal: true,
				}}, nil
			}

			okPayload, err := json.Marshal(map[string]any{
				"path":   path,
				"owner":  p.Owner,
				"repo":   p.Repo,
				"number": p.Number,
				"json":   json.RawMessage(stdout),
			})
			if err != nil {
				return nil, err
			}
			return []event.Event{{
				Type:    projection.TypeGhQueryResult,
				Payload: okPayload,
			}}, nil
		},
	}, nil
}
