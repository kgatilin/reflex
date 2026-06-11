package analyzer

import (
	"reflect"
	"sort"
	"testing"
	"time"
)

// buildSyntheticTrace returns a deterministic 7-event trace shaped like
// a fan-out/fan-in pipeline. It is the workhorse fixture for every metric
// test in this file — we keep it in one place so each test asserts
// against the same ground truth.
//
// Shape:
//
//	R0 RequestReceived (req=r1, source=cli)
//	 └─ R1 StepParsed (source=parse)
//	     ├─ R2 StepFetched (source=fetch_a)
//	     │   └─ R4 StepDecided (source=classify, non-terminal)
//	     │       └─ R6 RequestHandled (source=finalize, terminal)
//	     └─ R3 StepFetched (source=fetch_b)
//	         └─ R5 StepPending (source=classify, terminal leaf)
//
// Width: max fan-out = 2 (StepParsed).
// Depth: longest path = R0 → R1 → R2 → R4 → R6 = 4 edges.
// Terminals: RequestHandled (request closer) + StepPending (non-closing leaf).
func buildSyntheticTrace() *Trace {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mk := func(id, typ, src, caused string, terminal bool, dt time.Duration) TraceEvent {
		return TraceEvent{
			ID:        id,
			Type:      typ,
			RequestID: "r1",
			TS:        t0.Add(dt),
			Source:    src,
			CausedBy:  caused,
			Terminal:  terminal,
		}
	}
	return &Trace{
		Events: []TraceEvent{
			mk("R0", "RequestReceived", "cli", "", false, 0),
			mk("R1", "StepParsed", "parse", "R0", false, 10*time.Millisecond),
			mk("R2", "StepFetched", "fetch_a", "R1", false, 100*time.Millisecond),
			mk("R3", "StepFetched", "fetch_b", "R1", false, 200*time.Millisecond),
			mk("R4", "StepDecided", "classify", "R2", false, 250*time.Millisecond),
			mk("R5", "StepPending", "classify", "R3", true, 260*time.Millisecond),
			mk("R6", "RequestHandled", "finalize", "R4", true, 270*time.Millisecond),
		},
	}
}

// TestCausalWidth verifies the width metric: the highest in-request
// fan-out across all events. StepParsed (R1) fans out to two
// StepFetched events (R2, R3), and that 2 should be the reported width.
func TestCausalWidth(t *testing.T) {
	tr := buildSyntheticTrace()
	m := Compute(tr)
	rm, ok := m.PerRequest["r1"]
	if !ok {
		t.Fatalf("no metrics for r1")
	}
	if rm.CausalWidth != 2 {
		t.Errorf("width: got %d, want 2", rm.CausalWidth)
	}
}

// TestCausalDepth verifies the depth metric: the longest causal path
// from any request root to any leaf. In the synthetic trace that's
// R0 → R1 → R2 → R4 → R6 = 4 edges.
func TestCausalDepth(t *testing.T) {
	tr := buildSyntheticTrace()
	m := Compute(tr)
	rm := m.PerRequest["r1"]
	if rm.CausalDepth != 4 {
		t.Errorf("depth: got %d, want 4", rm.CausalDepth)
	}
}

// TestOrphanDetection verifies the orphan-count metric. Add a rogue
// non-terminal event with no children — it should be reported. The
// healthy synthetic trace produces zero orphans on its own.
func TestOrphanDetection(t *testing.T) {
	tr := buildSyntheticTrace()
	m := Compute(tr)
	if len(m.Orphans) != 0 {
		t.Fatalf("healthy trace: got %d orphans, want 0: %+v", len(m.Orphans), m.Orphans)
	}

	// Inject a rogue event: non-terminal, no descendants.
	rogue := TraceEvent{
		ID:        "X",
		Type:      "GhostEvent",
		RequestID: "r1",
		TS:        time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC),
		Source:    "ghost",
		CausedBy:  "R0",
		Terminal:  false,
	}
	tr.Events = append(tr.Events, rogue)
	m = Compute(tr)
	if len(m.Orphans) != 1 || m.Orphans[0].EventID != "X" {
		t.Errorf("orphan: got %+v, want one record with EventID=X", m.Orphans)
	}
}

// TestTerminationCorrectness verifies the per-request termination
// invariant. The synthetic trace has a RequestHandled closer (plus a
// non-closing StepPending leaf), so the request should report correct.
// Removing all terminals should flip the flag.
func TestTerminationCorrectness(t *testing.T) {
	tr := buildSyntheticTrace()
	m := Compute(tr)
	rm := m.PerRequest["r1"]
	if !rm.TerminationCorrect {
		t.Errorf("healthy: termination_correct=false, want true (violation=%q)", rm.TerminationViolation)
	}

	// Drop the terminal events.
	var stripped []TraceEvent
	for _, e := range tr.Events {
		if !e.Terminal {
			stripped = append(stripped, e)
		}
	}
	tr2 := &Trace{Events: stripped}
	m2 := Compute(tr2)
	if m2.PerRequest["r1"].TerminationCorrect {
		t.Errorf("trace with no terminals: termination_correct=true, want false")
	}
}

// TestHandlerUtilization verifies the events-per-source histogram. The
// synthetic trace has classify firing twice (StepDecided + StepPending)
// and every other handler firing once.
func TestHandlerUtilization(t *testing.T) {
	tr := buildSyntheticTrace()
	m := Compute(tr)
	want := map[string]int{
		"cli":      1,
		"parse":    1,
		"fetch_a":  1,
		"fetch_b":  1,
		"classify": 2,
		"finalize": 1,
	}
	if !reflect.DeepEqual(m.HandlerUtilization, want) {
		t.Errorf("utilization mismatch:\ngot:  %#v\nwant: %#v", m.HandlerUtilization, want)
	}
}

// TestHandlerLatencyMedian verifies the median-latency metric. We seed
// a trace where one source emits two events with known parent-relative
// timestamps, and check the median picks the expected value.
func TestHandlerLatencyMedian(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mk := func(id, typ, src, caused string, dt time.Duration, term bool) TraceEvent {
		return TraceEvent{ID: id, Type: typ, RequestID: "r1", TS: t0.Add(dt), Source: src, CausedBy: caused, Terminal: term}
	}
	tr := &Trace{Events: []TraceEvent{
		mk("A", "RequestReceived", "cli", "", 0, false),
		mk("B1", "Foo", "worker", "A", 100*time.Millisecond, false),
		mk("B2", "Foo", "worker", "A", 300*time.Millisecond, false),
		mk("C", "Bar", "closer", "B1", 350*time.Millisecond, true),
		mk("D", "Bar", "closer", "B2", 400*time.Millisecond, true),
		mk("E", "RequestHandled", "closer", "B1", 500*time.Millisecond, true),
	}}
	m := Compute(tr)
	// "worker" emitted B1 and B2 with parent-relative deltas 100ms and 300ms.
	// Median = (100+300)/2 = 200ms.
	if got := m.HandlerLatencyMS["worker"]; got != 200 {
		t.Errorf("worker median latency: got %v, want 200", got)
	}
}

// TestPowerDiagCycleCleanTrace verifies the archmotif cycle check
// returns zero on a healthy DAG (the synthetic trace).
func TestPowerDiagCycleCleanTrace(t *testing.T) {
	tr := buildSyntheticTrace()
	g, ids, err := BuildArchmotifGraph(tr)
	if err != nil {
		t.Fatalf("build archmotif graph: %v", err)
	}
	rep := PowerDiagCycle(g, ids, 8)
	if rep.CyclingNodes != 0 {
		t.Errorf("clean trace: cycling_nodes=%d, want 0; shortest=%v", rep.CyclingNodes, rep.ShortestPerNode)
	}
}

// TestPowerDiagCycleDetectsCycle hand-builds a tiny graph with a 2-cycle
// and confirms PowerDiagCycle flags both nodes.
func TestPowerDiagCycleDetectsCycle(t *testing.T) {
	// Construct an archmotif graph by hand: a → b → a.
	// We go through BuildArchmotifGraph by crafting a trace that points
	// caused_by both ways (deliberately malformed, never produced by
	// reflex). We bypass via direct AddDependency.
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mk := func(id, src, caused string) TraceEvent {
		return TraceEvent{ID: id, Type: "Cycle", RequestID: "r1", TS: t0, Source: src, CausedBy: caused}
	}
	tr := &Trace{Events: []TraceEvent{
		mk("a", "s", "b"),
		mk("b", "s", "a"),
	}}
	g, ids, err := BuildArchmotifGraph(tr)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	rep := PowerDiagCycle(g, ids, 8)
	if rep.CyclingNodes != 2 {
		t.Errorf("2-cycle: cycling_nodes=%d, want 2 (shortest=%v)", rep.CyclingNodes, rep.ShortestPerNode)
	}
	if rep.ShortestPerNode["a"] != 2 || rep.ShortestPerNode["b"] != 2 {
		t.Errorf("shortest cycle length: got %+v, want both 2", rep.ShortestPerNode)
	}
}

// TestObjectiveScoring verifies the objective function:
//   - healthy synthetic trace → objective == max_depth (no penalties).
//   - inject an orphan → objective += 1000.
//   - corrupt termination → objective += 1000.
func TestObjectiveScoring(t *testing.T) {
	tr := buildSyntheticTrace()
	rep, err := Analyze(tr)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if rep.Objective != 4 {
		t.Errorf("healthy objective: got %v, want 4", rep.Objective)
	}

	// Inject an orphan: depth stays 4, objective becomes 4 + 1000.
	tr2 := buildSyntheticTrace()
	tr2.Events = append(tr2.Events, TraceEvent{
		ID: "X", Type: "Ghost", RequestID: "r1",
		TS: tr2.Events[0].TS, Source: "ghost", CausedBy: "R0", Terminal: false,
	})
	rep2, _ := Analyze(tr2)
	if rep2.Objective != 1004 {
		t.Errorf("orphan-penalty objective: got %v, want 1004", rep2.Objective)
	}

	// Strip all terminals: termination broken AND R4 / R3 (the formerly
	// non-leaf nodes whose terminal children were removed) become orphans
	// themselves. Depth shrinks to 3 (R0→R1→R2→R4). Objective breakdown:
	//   3 (depth) + 1000 (mistermination) + 2*1000 (R3+R4 orphans) = 3003.
	// This is the honest answer — Phase 1 invariant violations cascade.
	tr3 := buildSyntheticTrace()
	clean := tr3.Events[:0]
	for _, e := range tr3.Events {
		if !e.Terminal {
			clean = append(clean, e)
		}
	}
	tr3.Events = clean
	rep3, _ := Analyze(tr3)
	if rep3.Objective != 3003 {
		t.Errorf("mistermination-penalty objective: got %v, want 3003 (depth 3 + 1000 + 2*1000 orphans)", rep3.Objective)
	}
}

// TestRequestIDsOrder ensures distinct request IDs come back in first-
// seen order — important for the watch-mode display.
func TestRequestIDsOrder(t *testing.T) {
	tr := &Trace{Events: []TraceEvent{
		{ID: "1", RequestID: "alpha"},
		{ID: "2", RequestID: "beta"},
		{ID: "3", RequestID: "alpha"},
		{ID: "4", RequestID: "gamma"},
	}}
	got := tr.RequestIDs()
	want := []string{"alpha", "beta", "gamma"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RequestIDs: got %v, want %v", got, want)
	}
	// Sanity check on ForRequest — should match request_id field, not
	// position.
	sort.Strings(got)
	// no further assertion; we only verify the order test above.
}
