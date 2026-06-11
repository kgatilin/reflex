package analyzer

import (
	"sort"
	"time"
)

// Metrics is the bundle of per-trace numeric / set-valued summaries the
// analyzer computes. It is the input to the objective function and the
// payload of the JSON report.
type Metrics struct {
	// PerRequest holds causal width/depth and termination correctness
	// for each request_id in the trace.
	PerRequest map[string]RequestMetrics `json:"per_request"`
	// Orphans lists non-terminal events with no children. Phase 1
	// invariant: this slice should be empty. Each entry is the offending
	// event's ID + type + request_id.
	Orphans []OrphanRecord `json:"orphans"`
	// HandlerUtilization counts events emitted per source (handler name).
	// A handler with 0 events probably means a config-time wiring bug.
	HandlerUtilization map[string]int `json:"handler_utilization"`
	// HandlerLatencyMS reports the median time-delta (ms) between the
	// event that triggered a handler (consume) and the events it emitted
	// (emit), grouped by handler source.
	HandlerLatencyMS map[string]float64 `json:"handler_latency_ms"`
	// TotalEvents is len(trace.Events) — a sanity counter.
	TotalEvents int `json:"total_events"`
}

// RequestMetrics groups per-request_id measurements.
type RequestMetrics struct {
	CausalWidth          int      `json:"causal_width"`
	CausalDepth          int      `json:"causal_depth"`
	TerminalCount        int      `json:"terminal_count"`
	TerminalTypes        []string `json:"terminal_types"`
	TerminationCorrect   bool     `json:"termination_correct"`
	TerminationViolation string   `json:"termination_violation,omitempty"`
}

// OrphanRecord describes a Phase 1 invariant violation: a non-terminal
// event that produced no children.
type OrphanRecord struct {
	EventID   string `json:"event_id"`
	EventType string `json:"event_type"`
	RequestID string `json:"request_id"`
	Source    string `json:"source"`
}

// requestClosingTerminals is the Phase 1 invariant set: every request_id
// must have exactly one terminal whose type is in this set, marking the
// request as closed. Other terminal events (EventOrphaned, LoopExhausted,
// …) are legal leaves in the middle of a trace but are not request
// closers — they don't satisfy the invariant on their own.
var requestClosingTerminals = map[string]bool{
	"RequestHandled":   true,
	"RequestUnhandled": true,
}

// childrenIndex returns map[parentID] → []TraceEvent of direct children
// (events whose caused_by == parent's id). The map is computed once and
// reused across metric calls — building it twice is wasted work on large
// traces.
func childrenIndex(events []TraceEvent) map[string][]TraceEvent {
	idx := map[string][]TraceEvent{}
	for _, e := range events {
		if e.CausedBy == "" {
			continue
		}
		idx[e.CausedBy] = append(idx[e.CausedBy], e)
	}
	return idx
}

// Compute returns the full Metrics bundle for a trace. Order of metric
// computation is arbitrary — each is independent of the others.
func Compute(t *Trace) Metrics {
	children := childrenIndex(t.Events)
	m := Metrics{
		PerRequest:         map[string]RequestMetrics{},
		HandlerUtilization: handlerUtilization(t.Events),
		HandlerLatencyMS:   handlerLatencyMS(t.Events, children),
		Orphans:            orphans(t.Events, children),
		TotalEvents:        len(t.Events),
	}
	for _, rid := range t.RequestIDs() {
		evs := t.ForRequest(rid)
		m.PerRequest[rid] = requestMetrics(rid, evs, children)
	}
	return m
}

// requestMetrics computes width/depth/termination correctness for one
// request_id. The graph is restricted to events in this request — the
// dispatcher invariant says caused_by always points within the same
// request, but we still defensively only follow edges that stay inside
// `byID`.
func requestMetrics(reqID string, evs []TraceEvent, children map[string][]TraceEvent) RequestMetrics {
	byID := map[string]TraceEvent{}
	for _, e := range evs {
		byID[e.ID] = e
	}

	// Width: max in-request fan-out at any node.
	width := 0
	for _, e := range evs {
		// Count only children that belong to this request.
		c := 0
		for _, ch := range children[e.ID] {
			if _, ok := byID[ch.ID]; ok {
				c++
			}
		}
		if c > width {
			width = c
		}
	}

	// Depth: longest path (in edges) from a root (no caused_by, or
	// caused_by points outside this request) to any leaf. Memoise.
	memo := map[string]int{}
	var depthFrom func(id string) int
	depthFrom = func(id string) int {
		if v, ok := memo[id]; ok {
			return v
		}
		best := 0
		for _, ch := range children[id] {
			if _, ok := byID[ch.ID]; !ok {
				continue
			}
			d := 1 + depthFrom(ch.ID)
			if d > best {
				best = d
			}
		}
		memo[id] = best
		return best
	}
	depth := 0
	for _, e := range evs {
		// Only start from request roots (no caused_by or caused_by-out-of-request).
		if e.CausedBy != "" {
			if _, ok := byID[e.CausedBy]; ok {
				continue
			}
		}
		d := depthFrom(e.ID)
		if d > depth {
			depth = d
		}
	}

	// Termination: collect terminal events. Phase 1 invariant requires
	// at least one — and the set of acceptable types matches a closed
	// request (RequestHandled / RequestUnhandled).
	var termTypes []string
	termCount := 0
	for _, e := range evs {
		if e.Terminal {
			termCount++
			termTypes = append(termTypes, e.Type)
		}
	}
	sort.Strings(termTypes)
	correct, violation := isTerminationCorrect(termTypes, termCount)
	_ = reqID // reqID is kept in the signature for future scoped diagnostics.

	return RequestMetrics{
		CausalWidth:          width,
		CausalDepth:          depth,
		TerminalCount:        termCount,
		TerminalTypes:        dedupStrings(termTypes),
		TerminationCorrect:   correct,
		TerminationViolation: violation,
	}
}

// isTerminationCorrect mirrors the Phase 1 invariant for a closed request:
// at least one of the terminal events must be a request-closer
// (RequestHandled / RequestUnhandled). Other terminal types (EventOrphaned,
// LoopExhausted) are legal leaves but do not by themselves close the
// request — the dispatcher's unhandled_watcher will emit RequestUnhandled
// if no closer appears, so the invariant "every request has at least one
// closing terminal" should hold for any complete trace.
//
// Returns (correct, violation-message). The violation message is empty
// when correct == true.
func isTerminationCorrect(types []string, count int) (bool, string) {
	if count == 0 {
		return false, "no terminal events"
	}
	for _, t := range types {
		if requestClosingTerminals[t] {
			return true, ""
		}
	}
	return false, "no request-closing terminal (RequestHandled/RequestUnhandled)"
}

// orphans walks every non-terminal event and flags those with zero
// children. Phase 1 invariant says this list should be empty.
//
// Special case: events emitted by `unhandled_watcher` have no caused_by
// (they fire on quiescence, not in response to a specific event) and are
// themselves terminal — they're not orphans.
func orphans(events []TraceEvent, children map[string][]TraceEvent) []OrphanRecord {
	var out []OrphanRecord
	for _, e := range events {
		if e.Terminal {
			continue
		}
		if len(children[e.ID]) == 0 {
			out = append(out, OrphanRecord{
				EventID:   e.ID,
				EventType: e.Type,
				RequestID: e.RequestID,
				Source:    e.Source,
			})
		}
	}
	return out
}

// handlerUtilization counts events grouped by source (handler name).
func handlerUtilization(events []TraceEvent) map[string]int {
	out := map[string]int{}
	for _, e := range events {
		if e.Source == "" {
			continue
		}
		out[e.Source]++
	}
	return out
}

// handlerLatencyMS returns the median time-delta in milliseconds between
// a handler's trigger (the consume event = event referenced by caused_by)
// and the events that handler emits, grouped by emitter source.
//
// The model: handler S consumed event P (where children[P.ID] contains C
// with C.Source == S), and the latency for that emission is C.TS − P.TS.
// We then take the median over all such (P, C) pairs per source S.
//
// Why median and not mean: the dispatcher is single-threaded, so latency
// is dominated by the slowest handler in the queue ahead of this one;
// outliers (one slow tool call) would dominate a mean. Median is the
// honest "typical" number.
func handlerLatencyMS(events []TraceEvent, children map[string][]TraceEvent) map[string]float64 {
	byID := map[string]TraceEvent{}
	for _, e := range events {
		byID[e.ID] = e
	}
	deltas := map[string][]float64{} // source → ms latencies
	for _, parent := range events {
		for _, child := range children[parent.ID] {
			if child.Source == "" {
				continue
			}
			d := child.TS.Sub(parent.TS)
			if d < 0 {
				// Clock skew or batched timestamps — clamp.
				d = 0
			}
			ms := float64(d) / float64(time.Millisecond)
			deltas[child.Source] = append(deltas[child.Source], ms)
		}
	}
	out := map[string]float64{}
	for source, ds := range deltas {
		out[source] = medianFloat(ds)
	}
	return out
}

// medianFloat returns the median of xs (allocating a copy so it doesn't
// reorder the caller's slice). Empty input returns 0.
func medianFloat(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	c := make([]float64, len(xs))
	copy(c, xs)
	sort.Float64s(c)
	mid := len(c) / 2
	if len(c)%2 == 1 {
		return c[mid]
	}
	return (c[mid-1] + c[mid]) / 2
}

// dedupStrings preserves order while removing duplicates. Used by the
// terminal-type list so identical types from different events collapse.
func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
