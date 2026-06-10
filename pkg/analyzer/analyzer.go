package analyzer

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// Report is the top-level analyzer output: metrics, archmotif cycle
// check, and the single objective scalar. JSON-serialisable for the
// --json flag and the watch loop's structured deltas.
type Report struct {
	// Source is the path the trace was read from (or "<stdin>"/empty for
	// in-memory traces).
	Source string `json:"source,omitempty"`
	// Metrics is the per-trace numeric bundle (see Metrics).
	Metrics Metrics `json:"metrics"`
	// Cycles is the power-diagonal cycle check over the archmotif-built
	// graph. CyclingNodes should be 0 for a healthy reflex trace.
	Cycles CycleReport `json:"cycles"`
	// Objective is the single scalar the optimisation loop minimises.
	// Smaller is better.
	Objective float64 `json:"objective"`
	// ObjectiveWeights records the penalty constants used so the loop's
	// "objective went down" deltas are reproducible.
	ObjectiveWeights ObjectiveWeights `json:"objective_weights"`
}

// Analyze runs the full analyzer pipeline over a trace: parse → metrics
// → archmotif graph construction → cycle check → objective. Pure
// function; no side effects.
func Analyze(t *Trace) (*Report, error) {
	m := Compute(t)
	g, ids, err := BuildArchmotifGraph(t)
	if err != nil {
		return nil, fmt.Errorf("archmotif build: %w", err)
	}
	cycles := PowerDiagCycle(g, ids, 8)
	weights := DefaultObjectiveWeights()
	return &Report{
		Source:           t.Source,
		Metrics:          m,
		Cycles:           cycles,
		Objective:        Objective(m, cycles, weights),
		ObjectiveWeights: weights,
	}, nil
}

// PrintText writes the report as a compact human-readable summary. The
// CLI's default output. Intentionally terse: trace, request count,
// objective + breakdown, any violations.
func (r *Report) PrintText(w io.Writer) {
	src := r.Source
	if src == "" {
		src = "<in-memory>"
	}
	fmt.Fprintf(w, "trace:               %s\n", src)
	fmt.Fprintf(w, "events:              %d\n", r.Metrics.TotalEvents)
	fmt.Fprintf(w, "requests:            %d\n", len(r.Metrics.PerRequest))
	fmt.Fprintf(w, "objective:           %g\n", r.Objective)
	fmt.Fprintf(w, "max_causal_depth:    %d\n", maxDepth(r.Metrics))
	fmt.Fprintf(w, "orphans:             %d\n", len(r.Metrics.Orphans))
	fmt.Fprintf(w, "cycling_nodes:       %d\n", r.Cycles.CyclingNodes)

	mis := 0
	for _, rm := range r.Metrics.PerRequest {
		if !rm.TerminationCorrect {
			mis++
		}
	}
	fmt.Fprintf(w, "mis-terminated_reqs: %d\n", mis)

	// Per-request table (sorted by request id for determinism).
	reqIDs := make([]string, 0, len(r.Metrics.PerRequest))
	for rid := range r.Metrics.PerRequest {
		reqIDs = append(reqIDs, rid)
	}
	sort.Strings(reqIDs)
	fmt.Fprintln(w, "\nper-request:")
	for _, rid := range reqIDs {
		rm := r.Metrics.PerRequest[rid]
		short := rid
		if len(short) > 8 {
			short = short[:8]
		}
		mark := "ok"
		if !rm.TerminationCorrect {
			mark = "FAIL: " + rm.TerminationViolation
		}
		fmt.Fprintf(w,
			"  %s  width=%d  depth=%d  terminals=%v  %s\n",
			short, rm.CausalWidth, rm.CausalDepth, rm.TerminalTypes, mark)
	}

	// Handler utilisation + latency (sorted by handler name).
	sources := make([]string, 0, len(r.Metrics.HandlerUtilization))
	for s := range r.Metrics.HandlerUtilization {
		sources = append(sources, s)
	}
	sort.Strings(sources)
	fmt.Fprintln(w, "\nhandler utilisation / latency:")
	for _, s := range sources {
		lat := r.Metrics.HandlerLatencyMS[s]
		fmt.Fprintf(w, "  %-24s  events=%-3d  median_latency_ms=%.2f\n",
			s, r.Metrics.HandlerUtilization[s], lat)
	}

	// Orphan / cycle detail blocks only when non-empty (keep healthy
	// output a single screen).
	if len(r.Metrics.Orphans) > 0 {
		fmt.Fprintln(w, "\norphan events (non-terminal with no children):")
		for _, o := range r.Metrics.Orphans {
			fmt.Fprintf(w, "  %s  %s  source=%s  request=%s\n",
				o.EventID, o.EventType, o.Source, o.RequestID)
		}
	}
	if r.Cycles.CyclingNodes > 0 {
		fmt.Fprintln(w, "\ncycle (power-diagonal):")
		ids := make([]string, 0, len(r.Cycles.ShortestPerNode))
		for id := range r.Cycles.ShortestPerNode {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			fmt.Fprintf(w, "  %s  shortest_cycle=%d\n",
				id, r.Cycles.ShortestPerNode[id])
		}
	}
}

// PrintJSON writes the report as indented JSON.
func (r *Report) PrintJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// FilterRequest returns a copy of r with PerRequest narrowed to the
// given request_id (or unchanged when reqID is empty). The CLI uses
// this when --request-id is set.
func (r *Report) FilterRequest(reqID string) *Report {
	if reqID == "" {
		return r
	}
	out := *r
	if rm, ok := r.Metrics.PerRequest[reqID]; ok {
		out.Metrics.PerRequest = map[string]RequestMetrics{reqID: rm}
	} else {
		out.Metrics.PerRequest = map[string]RequestMetrics{}
	}
	return &out
}
