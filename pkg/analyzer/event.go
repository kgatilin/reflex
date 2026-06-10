// Package analyzer is reflex's background analyzer engine: it reads a
// reflex event log (a JSON-Lines trace produced by `reflex run --trace-file`
// or `reflex run --trace`), constructs a causal DAG from it, and computes
// graph metrics + diagnostics over that DAG.
//
// The package is intentionally split into composable layers:
//
//   - event.go  — TraceEvent type and JSONL reader.
//   - metrics.go — pure metric functions over a parsed trace.
//   - archmotif_adapter.go — bridge that materialises the trace as an
//     archmotif typed graph via pkg/archmotifimport. The analyzer keeps
//     its own native representation (the metrics don't need archmotif)
//     and uses archmotif for graph-level architectural validators (today:
//     power-diagonal cycle check, mirroring archmotif's matrix_cycle
//     validator).
//   - objective.go — the single-number objective function the optimisation
//     loop will minimise. Phase 3 ships read-only computation; the loop is
//     a `--watch` mode that recomputes on file change and prints the delta.
//   - watch.go — directory-watch driver for the loop.
//   - analyzer.go — top-level Analyze(trace) → Report orchestrator.
//
// The handler-level graph from `pkg/graph` is conceptually distinct from
// the event-trace graph this package builds. The handler graph is static
// (config-time topology); the trace graph is dynamic (runtime instances of
// events, possibly multiple per handler firing). The analyzer can reference
// the handler graph in future phases for handler-vs-event cross-validation.
package analyzer

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// TraceEvent mirrors reflex's pkg/event.Event JSON shape with the fields
// the analyzer actually consumes. It is intentionally a local copy rather
// than a re-import of pkg/event.Event so the analyzer can read traces
// produced by any reflex build (incl. older or newer event schemas) — the
// json tags here pin the contract.
type TraceEvent struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	RequestID string          `json:"request_id"`
	TS        time.Time       `json:"ts"`
	Source    string          `json:"source"`
	CausedBy  string          `json:"caused_by,omitempty"`
	Terminal  bool            `json:"terminal"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// Trace is an ordered list of events read from a single reflex run.
// Order is preserved as it appeared on the log (append order); metrics
// that depend on causal structure traverse CausedBy rather than position.
type Trace struct {
	Events []TraceEvent
	// Source is the path the trace was read from, for diagnostics.
	// Empty when the trace was constructed in memory (tests).
	Source string
}

// ReadTraceFile reads a JSONL event trace from path. Each non-empty line
// must be a JSON object matching TraceEvent. Lines that look like
// non-JSON output (do not start with `{`) are skipped silently — that
// makes the reader tolerant of `--trace` (stdout-mixed) input as well as
// the clean `--trace-file` form.
//
// Skipping non-JSON is a deliberate concession to humans who pipe
// `reflex run --trace ... | analyzer`. We document the recommendation
// (use --trace-file) but don't reject the alternative.
func ReadTraceFile(path string) (*Trace, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open trace %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	tr, err := ReadTrace(f)
	if err != nil {
		return nil, fmt.Errorf("read trace %s: %w", path, err)
	}
	tr.Source = path
	return tr, nil
}

// ReadTrace reads a JSONL trace from an io.Reader. See ReadTraceFile for
// the mixed-input tolerance rule.
func ReadTrace(r io.Reader) (*Trace, error) {
	scanner := bufio.NewScanner(r)
	// Trace lines can be wide (some GhQueryResult payloads inline the full
	// JSON body of a GitHub timeline response). Lift the buffer ceiling.
	const maxLine = 16 * 1024 * 1024 // 16 MiB
	scanner.Buffer(make([]byte, 64*1024), maxLine)
	tr := &Trace{}
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		b := scanner.Bytes()
		// Skip blank lines and obvious non-JSON (the printer handler's
		// "triage: ..." line, etc).
		trimmed := skipLeadingSpace(b)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			continue
		}
		var ev TraceEvent
		if err := json.Unmarshal(b, &ev); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		tr.Events = append(tr.Events, ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return tr, nil
}

// skipLeadingSpace returns b with leading ASCII spaces/tabs removed.
// Tiny helper to avoid pulling strings.TrimLeft just for two byte
// classes.
func skipLeadingSpace(b []byte) []byte {
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\t') {
		i++
	}
	return b[i:]
}

// RequestIDs returns the distinct request IDs in the trace, in first-seen
// order. The dispatcher fires events in append order, so this preserves
// the natural request sequence.
func (t *Trace) RequestIDs() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, e := range t.Events {
		if e.RequestID == "" {
			continue
		}
		if !seen[e.RequestID] {
			seen[e.RequestID] = true
			out = append(out, e.RequestID)
		}
	}
	return out
}

// ForRequest returns events with the given request ID, preserving order.
func (t *Trace) ForRequest(reqID string) []TraceEvent {
	var out []TraceEvent
	for _, e := range t.Events {
		if e.RequestID == reqID {
			out = append(out, e)
		}
	}
	return out
}
