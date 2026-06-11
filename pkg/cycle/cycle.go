// Package cycle holds the shared cycle-detection algorithm used by both
// the static (YAML) handler-graph pre-check and the runtime live-table
// check.
//
// Both call sites previously hand-rolled their own Tarjan SCC over their
// own private adjacency structure. The two implementations were nearly
// identical and drifted in trivial ways (sorting order, self-loop checks,
// the meaning of "capped"). DetectUncappedCycle takes a transport-neutral
// list of Edge values and returns the first uncapped cycle found, if any.
//
// Edges with Capped=true are still traversed (they participate in the
// graph) but a SCC containing any capped edge is considered acceptable —
// the cap will be enforced at runtime, so the cycle is not a structural
// error. Callers map their own notion of "capped" (loop_caps,
// MaxIterations on a subscription, etc.) onto this single flag.
//
// Terminal edges should be filtered by the caller BEFORE handing the edge
// list to DetectUncappedCycle: a terminal emission cannot close a runtime
// cycle by construction, so it is dropped from the adjacency rather than
// modelled as a separate flag here.
package cycle

import "sort"

// Edge is one directed edge in a cycle-detection graph. From and To are
// opaque node identifiers; Capped is true when the cycle this edge
// participates in carries an explicit upper bound (a loop cap, a per-
// handler MaxIterations, etc.) and so should not be reported as a
// structural error.
type Edge struct {
	From   string
	To     string
	Capped bool
}

// DetectUncappedCycle returns the first uncapped cycle in the directed
// graph described by edges, in sorted-node order. A cycle is a strongly-
// connected component with either more than one node or a self-loop. A
// cycle is "capped" iff at least one node in the SCC is the source of a
// Capped edge inside that SCC; such cycles are considered acceptable.
//
// Returns (cycle, true) when an uncapped cycle exists; (nil, false)
// otherwise. The returned cycle is sorted lexicographically by node name
// for deterministic output.
func DetectUncappedCycle(edges []Edge) ([]string, bool) {
	adj, cappedNode := buildAdj(edges)
	sccs := stronglyConnectedComponents(adj)
	for _, scc := range sccs {
		if !sccIsCycle(scc, adj) {
			continue
		}
		if sccIsCapped(scc, cappedNode) {
			continue
		}
		out := append([]string(nil), scc...)
		sort.Strings(out)
		return out, true
	}
	return nil, false
}

// FindCycles returns every non-trivial SCC (cycles) in the directed
// graph described by edges, sorted deterministically. Each returned SCC
// has its members sorted lexicographically; the SCC list itself is
// sorted by first member. Caller can decide which are acceptable by
// inspecting per-cycle properties via its own data.
//
// Unlike DetectUncappedCycle, FindCycles ignores the Capped flag — it
// returns the full structural picture, including capped cycles. The
// static-graph pre-check uses this to populate HandlerGraph.Cycles
// (which must include capped cycles too, so `reflex describe` can
// surface them as informational SCCs).
func FindCycles(edges []Edge) [][]string {
	adj, _ := buildAdj(edges)
	sccs := stronglyConnectedComponents(adj)
	out := make([][]string, 0, len(sccs))
	for _, scc := range sccs {
		if !sccIsCycle(scc, adj) {
			continue
		}
		out = append(out, scc)
	}
	return out
}

func buildAdj(edges []Edge) (map[string][]string, map[string]bool) {
	adj := map[string][]string{}
	cappedNode := map[string]bool{}
	for _, e := range edges {
		adj[e.From] = append(adj[e.From], e.To)
		if _, ok := adj[e.To]; !ok {
			adj[e.To] = nil
		}
		if e.Capped {
			cappedNode[e.From] = true
		}
	}
	return adj, cappedNode
}

// stronglyConnectedComponents runs the textbook iterative-recursion form
// of Tarjan's SCC algorithm over an adjacency map. Nodes are visited in
// deterministic (sorted) order so the output is reproducible regardless
// of input ordering.
func stronglyConnectedComponents(adj map[string][]string) [][]string {
	index := 0
	indexOf := map[string]int{}
	lowlink := map[string]int{}
	onStack := map[string]bool{}
	stack := []string{}
	var sccs [][]string

	var strongConnect func(v string)
	strongConnect = func(v string) {
		indexOf[v] = index
		lowlink[v] = index
		index++
		stack = append(stack, v)
		onStack[v] = true

		for _, w := range adj[v] {
			if _, seen := indexOf[w]; !seen {
				strongConnect(w)
				if lowlink[w] < lowlink[v] {
					lowlink[v] = lowlink[w]
				}
			} else if onStack[w] {
				if indexOf[w] < lowlink[v] {
					lowlink[v] = indexOf[w]
				}
			}
		}

		if lowlink[v] == indexOf[v] {
			var scc []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				scc = append(scc, w)
				if w == v {
					break
				}
			}
			sccs = append(sccs, scc)
		}
	}

	names := make([]string, 0, len(adj))
	for n := range adj {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, v := range names {
		if _, seen := indexOf[v]; !seen {
			strongConnect(v)
		}
	}

	// Deterministic ordering between SCCs: sort each SCC's members, then
	// sort the SCC list by the (sorted) first member.
	for i := range sccs {
		sort.Strings(sccs[i])
	}
	sort.Slice(sccs, func(i, j int) bool {
		return sccs[i][0] < sccs[j][0]
	})
	return sccs
}

// sccIsCycle reports whether an SCC represents a real cycle: either more
// than one node, or a self-loop on its only node.
func sccIsCycle(scc []string, adj map[string][]string) bool {
	if len(scc) > 1 {
		return true
	}
	if len(scc) == 0 {
		return false
	}
	v := scc[0]
	for _, w := range adj[v] {
		if w == v {
			return true
		}
	}
	return false
}

// sccIsCapped reports whether any node in scc was the source of a capped
// edge — the caller's signal that the cycle has an explicit runtime
// upper bound and so is not a structural error.
func sccIsCapped(scc []string, cappedNode map[string]bool) bool {
	for _, v := range scc {
		if cappedNode[v] {
			return true
		}
	}
	return false
}
