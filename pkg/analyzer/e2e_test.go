package analyzer

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEndToEnd_SyntheticJSONL is the e2e check the brief asks for:
// write a synthetic trace as JSONL to disk, read it back through
// ReadTraceFile, run Analyze, and assert the report's objective and
// key fields. Catches regressions in the round-trip (reader, metrics,
// archmotif adapter, objective) as one combined check.
func TestEndToEnd_SyntheticJSONL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trace.jsonl")

	tr := buildSyntheticTrace()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create temp jsonl: %v", err)
	}
	enc := json.NewEncoder(f)
	for _, e := range tr.Events {
		if err := enc.Encode(e); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Round-trip via ReadTraceFile + Analyze.
	loaded, err := ReadTraceFile(path)
	if err != nil {
		t.Fatalf("ReadTraceFile: %v", err)
	}
	if loaded.Source != path {
		t.Errorf("source: got %q, want %q", loaded.Source, path)
	}
	if len(loaded.Events) != len(tr.Events) {
		t.Fatalf("event count: got %d, want %d", len(loaded.Events), len(tr.Events))
	}
	rep, err := Analyze(loaded)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	// The synthetic trace is healthy: objective should be the depth (4),
	// no orphans, no cycles, termination correct.
	if rep.Objective != 4 {
		t.Errorf("objective: got %v, want 4", rep.Objective)
	}
	if len(rep.Metrics.Orphans) != 0 {
		t.Errorf("orphans: %+v", rep.Metrics.Orphans)
	}
	if rep.Cycles.CyclingNodes != 0 {
		t.Errorf("cycles: %d", rep.Cycles.CyclingNodes)
	}
	rm, ok := rep.Metrics.PerRequest["r1"]
	if !ok || !rm.TerminationCorrect {
		t.Errorf("per-request[r1] termination_correct=false (%+v)", rm)
	}

	// PrintText must produce a non-empty summary.
	var buf bytes.Buffer
	rep.PrintText(&buf)
	if !strings.Contains(buf.String(), "objective:") {
		t.Errorf("PrintText missing 'objective:' header:\n%s", buf.String())
	}
}

// TestReadTraceTolerantOfStdoutMixing exercises the reader's tolerance
// for the printer handler's "triage: ..." lines mixed in with JSONL.
// Required when humans pipe `reflex run --trace ... | analyzer`.
func TestReadTraceTolerantOfStdoutMixing(t *testing.T) {
	input := strings.Join([]string{
		`triage: label_age=267h, kira=0 → STUCK`,
		`{"id":"A","type":"RequestReceived","request_id":"r1","ts":"2026-01-01T00:00:00Z","source":"cli","terminal":false}`,
		``,
		`some other human line`,
		`{"id":"B","type":"RequestHandled","request_id":"r1","ts":"2026-01-01T00:00:01Z","source":"finalize","caused_by":"A","terminal":true}`,
	}, "\n")

	tr, err := ReadTrace(strings.NewReader(input))
	if err != nil {
		t.Fatalf("ReadTrace: %v", err)
	}
	if len(tr.Events) != 2 {
		t.Errorf("expected 2 events (skipped non-JSON), got %d", len(tr.Events))
	}
	if tr.Events[0].ID != "A" || tr.Events[1].ID != "B" {
		t.Errorf("event order/ids wrong: %+v", tr.Events)
	}
}

// TestPerRequestFilterIsolation verifies FilterRequest narrows the
// report's PerRequest map cleanly without mutating the original.
func TestPerRequestFilterIsolation(t *testing.T) {
	// Two requests, both healthy.
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mk := func(id, typ, src, caused, rid string, dt time.Duration, term bool) TraceEvent {
		return TraceEvent{ID: id, Type: typ, RequestID: rid, TS: t0.Add(dt), Source: src, CausedBy: caused, Terminal: term}
	}
	tr := &Trace{Events: []TraceEvent{
		mk("A", "RequestReceived", "cli", "", "r1", 0, false),
		mk("B", "RequestHandled", "x", "A", "r1", 1, true),
		mk("C", "RequestReceived", "cli", "", "r2", 2, false),
		mk("D", "RequestHandled", "x", "C", "r2", 3, true),
	}}
	rep, err := Analyze(tr)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	filtered := rep.FilterRequest("r2")
	if len(filtered.Metrics.PerRequest) != 1 {
		t.Errorf("filter r2: got %d entries, want 1", len(filtered.Metrics.PerRequest))
	}
	if _, ok := filtered.Metrics.PerRequest["r2"]; !ok {
		t.Errorf("filter missing r2")
	}
	if len(rep.Metrics.PerRequest) != 2 {
		t.Errorf("filter mutated original: %d entries", len(rep.Metrics.PerRequest))
	}
}
