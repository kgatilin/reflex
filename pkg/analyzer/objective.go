package analyzer

import "math"

// Objective picked for Phase 3 (per the build brief):
//
//   «minimise causal depth subject to no orphans + correct termination»
//
// We encode that as a single scalar `Value` where smaller is better:
//
//   Value = max(per-request causal_depth) + 1000 * orphan_count
//           + 1000 * mis-terminated_requests + 1000 * cycling_nodes
//
// The 1000× penalties make any architectural-invariant violation
// (orphan / bad-termination / cycle) dwarf any plausible depth value, so
// "objective went down" always means "either the graph stayed valid
// and got shallower, or a violation was repaired". A future phase can
// switch from this penalty form to a hard-feasibility filter once the
// optimisation loop actually mutates the config.
//
// Why max-depth and not mean-depth: reflex requests are independent;
// the user-visible "how slow was the worst path" matters more than the
// average. If we ever batch many traces, we can swap in a different
// reducer here without touching the rest of the pipeline.
//
// Why 1000 specifically: trace depths for typical reflex graphs are in
// the 3–5 range; one violation should dominate ~200 perfectly-shaped
// traces. 1000 is "round number, comfortably larger than any plausible
// depth on a small reflex graph". A future phase that runs the optimiser
// over a real workload should re-tune this — the constant lives in one
// place by design.

// ObjectiveWeights configures the Objective(...) calculation. Defaults
// implement the 1000× penalty form described above; tests and the watch
// loop both round-trip through the same struct.
type ObjectiveWeights struct {
	OrphanPenalty       float64
	MisterminatePenalty float64
	CyclePenalty        float64
}

// DefaultObjectiveWeights returns the production weights. Exposed so the
// CLI can print them in the JSON report (config transparency).
func DefaultObjectiveWeights() ObjectiveWeights {
	return ObjectiveWeights{
		OrphanPenalty:       1000,
		MisterminatePenalty: 1000,
		CyclePenalty:        1000,
	}
}

// Objective combines a Metrics bundle and a CycleReport into the single
// scalar described above. A NaN/Inf protection guard returns +Inf when
// the input doesn't yield a finite number (shouldn't happen but defensive
// against future metric additions).
func Objective(m Metrics, cycles CycleReport, w ObjectiveWeights) float64 {
	depth := maxDepth(m)
	misterminated := 0
	for _, r := range m.PerRequest {
		if !r.TerminationCorrect {
			misterminated++
		}
	}
	v := float64(depth) +
		w.OrphanPenalty*float64(len(m.Orphans)) +
		w.MisterminatePenalty*float64(misterminated) +
		w.CyclePenalty*float64(cycles.CyclingNodes)
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return math.Inf(1)
	}
	return v
}

// maxDepth picks the largest CausalDepth across all requests in the
// trace. Empty trace returns 0.
func maxDepth(m Metrics) int {
	best := 0
	for _, r := range m.PerRequest {
		if r.CausalDepth > best {
			best = r.CausalDepth
		}
	}
	return best
}
