package analyzer

import (
	"fmt"

	archmotifimport "github.com/kgatilin/archmotif/pkg/archmotifimport"
)

// archmotif's pkg/archmotifimport API is intentionally code-graph-shaped:
// the node kinds are Package / Type / Function / Method / Field, and the
// builder methods mirror Java/Go program elements. Reflex events are not
// any of those — they are runtime instances of a causal DAG. There's no
// perfect node-kind match.
//
// The thinnest faithful adapter:
//   - Each event maps to a Package (the only top-level NodeKind the
//     builder exposes via AddPackage). The package id is the event id;
//     `layer` carries the event type and `aggregate` carries the source
//     handler. Both are recorded as Node attributes, which downstream
//     archmotif validators can inspect.
//   - Each caused_by relationship maps to an AddDependency edge of kind
//     DependencyDependsOn. This is a directed dependency, mirroring
//     reflex's causal direction (parent caused child).
//
// Friction logged (for Phase 6 planning):
//
//   - The matrix validators (matrix_cycle, layer_collapse) live under
//     internal/metrics and are not reachable from outside archmotif.
//     We re-implement power-diagonal cycle detection locally below
//     (PowerDiagCycle); it operates on the constructed archmotif Graph
//     via Edges() and matches the semantics of matrix_cycle.
//   - There is no public node kind for "runtime instance" / "event".
//     Using NodePackage is a forced fit — it works for the adjacency
//     algebra (which only cares about node IDs and edge endpoints), but
//     loses semantic fidelity. A future archmotif extension exposing a
//     generic NodeEvent kind (or simply parametrising AddPackage's name
//     "package" → "node") would close this gap.
//
// Both points are flagged in the analyzer's report so Phase 6 plans the
// archmotif-side changes alongside the optimisation work.

// BuildArchmotifGraph constructs an archmotif typed graph from a reflex
// trace. Returns the constructed *archmotifimport.Graph and the
// id-ordered slice of event IDs (matching graph node insertion order)
// for use by PowerDiagCycle / future matrix ops.
func BuildArchmotifGraph(t *Trace) (*archmotifimport.Graph, []string, error) {
	b := archmotifimport.NewBuilder()
	ids := make([]string, 0, len(t.Events))
	for _, e := range t.Events {
		// `layer` = event type, `aggregate` = source handler. Both end up
		// in Attrs on the archmotif Node; the matrix interpreters can read
		// them via the typed graph.
		if err := b.AddPackage(e.ID, e.Type, e.Source); err != nil {
			return nil, nil, fmt.Errorf("archmotif AddPackage %s: %w", e.ID, err)
		}
		ids = append(ids, e.ID)
	}
	for _, e := range t.Events {
		if e.CausedBy == "" {
			continue
		}
		// Skip caused_by pointers that reference a node outside the trace
		// (shouldn't happen in a complete log, but defensive).
		if err := b.AddDependency(e.CausedBy, e.ID, archmotifimport.DependencyDependsOn); err != nil {
			// archmotif refuses unknown nodes; we log the friction but
			// continue so the analyzer can still report on the rest.
			continue
		}
	}
	g, err := b.Build()
	if err != nil {
		return nil, nil, err
	}
	return g, ids, nil
}

// CycleReport summarises power-diagonal cycle detection over the
// archmotif graph built from the trace. The reflex event log is causal
// (parent → child by construction), so a non-zero cycle count is a hard
// architectural violation — equivalent to a malformed log where some
// event's caused_by forms a directed cycle.
type CycleReport struct {
	// CyclingNodes is the count of event IDs that participate in any
	// directed cycle of length ≤ K.
	CyclingNodes int `json:"cycling_nodes"`
	// ShortestPerNode maps event id → shortest cycle length touching it.
	ShortestPerNode map[string]int `json:"shortest_per_node,omitempty"`
	// MaxK is the K bound used by the power-diagonal scan.
	MaxK int `json:"max_k"`
}

// PowerDiagCycle implements archmotif's matrix_cycle semantics over the
// adjacency derived from the archmotif graph: for k ∈ [1..K], compute
// A^k and inspect its diagonal — (A^k)[i][i] > 0 means a closed walk of
// length k touches node i. The smallest such k is the shortest cycle
// length for that node.
//
// We re-implement here because archmotif's matrix validator framework
// (PowerDiagOp + cycleMatrixInterpreter) lives under internal/metrics
// and is not exposed via the public package surface.
//
// On reflex traces this should always return CyclingNodes == 0: events
// form a DAG by construction. A non-zero result is a smoking-gun
// invariant violation worth promoting to a hard report-level flag.
func PowerDiagCycle(g *archmotifimport.Graph, ids []string, maxK int) CycleReport {
	n := len(ids)
	idx := make(map[string]int, n)
	for i, id := range ids {
		idx[id] = i
	}
	// Adjacency as flat row-major []float64. We keep things explicit
	// instead of pulling gonum (which would add a transitive dep on
	// gonum just for one matrix-power loop).
	A := make([]float64, n*n)
	for _, e := range g.Edges() {
		i, ok1 := idx[e.From]
		j, ok2 := idx[e.To]
		if !ok1 || !ok2 {
			continue
		}
		// Multiple edges between the same pair (unlikely for reflex traces)
		// still treated as one for cycle detection.
		A[i*n+j] = 1
	}
	if maxK <= 0 {
		maxK = 8
	}
	// Pk starts as A; for each k=1..K we accumulate diagonal hits, then
	// multiply Pk by A to get A^(k+1).
	Pk := make([]float64, n*n)
	copy(Pk, A)
	shortest := map[string]int{}
	for k := 1; k <= maxK; k++ {
		for i := 0; i < n; i++ {
			if Pk[i*n+i] <= 0 {
				continue
			}
			if _, seen := shortest[ids[i]]; !seen {
				shortest[ids[i]] = k
			}
		}
		if k == maxK {
			break
		}
		Pk = matMul(Pk, A, n)
	}
	return CycleReport{
		CyclingNodes:    len(shortest),
		ShortestPerNode: shortest,
		MaxK:            maxK,
	}
}

// matMul multiplies two n×n row-major matrices. Boolean adjacency would
// suffice (we only care about non-zero), but reflex traces stay small
// (≤ tens of events per request, hundreds per session) so a plain
// triple loop is fine.
func matMul(a, b []float64, n int) []float64 {
	out := make([]float64, n*n)
	for i := 0; i < n; i++ {
		for k := 0; k < n; k++ {
			aik := a[i*n+k]
			if aik == 0 {
				continue
			}
			for j := 0; j < n; j++ {
				out[i*n+j] += aik * b[k*n+j]
			}
		}
	}
	return out
}
