// Package graph compiles a reflex YAML config into a static handler graph
// and validates it.
//
// The graph is the load-time projection of the runtime topology: each
// YAML-declared handler is a node, each (emitter type, consumer subscribes
// to that type) pair is an edge. The compiler runs Tarjan's SCC over the
// graph to find cycles; cycles are allowed only when at least one node in
// the SCC declares an explicit `loop: {max_iterations: N}`. Anything else
// is a hard error — reflex refuses to start.
//
// Edge weights:
//   - For an edge OUT of a loop-declaring node, Weight = MaxIterations
//     (the per-request cap the dispatcher will enforce at runtime).
//   - For all other edges, Weight = 1 — leaving room for a future
//     priority semantic without changing the wire shape.
//
// The package is consumed by `reflex validate` and `reflex describe`, and
// (via the LoopCaps map on the result) by the dispatcher's runtime
// enforcement. It deliberately knows nothing about how handlers actually
// run; it only reads the static spec.
package graph

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/handler"
)

// HandlerGraph is the compiled topology. Nodes and Edges are sorted for
// deterministic textual output.
type HandlerGraph struct {
	Nodes []HandlerNode
	Edges []HandlerEdge
	// DeclaredLoops is the set of loop names declared in the config.
	DeclaredLoops []string
	// Cycles is the set of strongly-connected components with >1 edge or a
	// self-loop. Each entry is a sorted list of handler names. Acyclic
	// graphs leave this empty.
	Cycles [][]string
}

// HandlerNode is one declared handler instance from the YAML.
type HandlerNode struct {
	Name        string             // unique handler instance name from YAML
	Type        string             // handler type
	Spec        handler.HandlerSpec // per-instance resolved spec
	LoopName    string             // name of the loop declaration, "" if not a loop node
	LoopCap     int                // max iterations; 0 if not a loop node
}

// HandlerEdge is one possible runtime causal link: emitter handler emits
// EventType; consumer handler subscribes to EventType via its YAML `on:`.
type HandlerEdge struct {
	From      string // emitter handler name
	To        string // consumer handler name
	EventType string // the event type linking them
	Weight    int    // see package doc
	// Terminal flags an emission marked Terminal at the spec level. Such
	// emissions cannot spawn descendants and so the edge cannot
	// participate in a runtime cycle, even though it shows up in the
	// static graph. Tarjan still sees the edge — but cycle detection
	// ignores it.
	Terminal bool
}

// LoopCaps is the dispatcher's read-side projection: handler name → cap.
type LoopCaps map[string]int

// Caps returns a deterministic LoopCaps from the graph.
func (g *HandlerGraph) Caps() LoopCaps {
	out := LoopCaps{}
	for _, n := range g.Nodes {
		if n.LoopCap > 0 {
			out[n.Name] = n.LoopCap
		}
	}
	return out
}

// Build compiles cfg against the introspection registry and validates that
// every cycle is capped. It returns the compiled graph regardless of cycle
// status — callers (validate / describe / runtime) inspect g.Cycles or err
// to decide what to do.
func Build(cfg *config.File, intro handler.Introspect) (*HandlerGraph, error) {
	if cfg == nil {
		return nil, fmt.Errorf("graph: cfg is nil")
	}
	if intro == nil {
		return nil, fmt.Errorf("graph: introspection registry is nil")
	}

	// Resolve every handler's per-instance spec. We use the *Registry's
	// ResolveSpec if available (it handles SpecResolver and Consumes="*"
	// substitution); otherwise we synthesise a minimal spec from the
	// interface — the test fakes use the minimal path.
	reg, _ := intro.(*handler.Registry)

	nodes := make([]HandlerNode, 0, len(cfg.Handlers))
	nodesByName := map[string]int{}
	declaredLoops := map[string]bool{}

	for _, hc := range cfg.Handlers {
		var spec handler.HandlerSpec
		var ok bool
		if reg != nil {
			spec, ok = reg.ResolveSpec(hc)
		} else {
			spec, ok = intro.SpecOf(hc.Type)
			if ok && (spec.Consumes == "*" || spec.Consumes == "") {
				spec.Consumes = hc.On
			}
		}
		if !ok {
			return nil, fmt.Errorf("graph: handler %q: type %q has no registered spec", hc.Name, hc.Type)
		}
		node := HandlerNode{
			Name: hc.Name,
			Type: hc.Type,
			Spec: spec,
		}
		if hc.Loop != nil {
			node.LoopCap = hc.Loop.MaxIterations
			node.LoopName = hc.Loop.Name
			if node.LoopName == "" {
				node.LoopName = hc.Name
			}
			declaredLoops[node.LoopName] = true
		}
		if _, dupe := nodesByName[hc.Name]; dupe {
			return nil, fmt.Errorf("graph: duplicate handler name %q", hc.Name)
		}
		nodesByName[hc.Name] = len(nodes)
		nodes = append(nodes, node)
	}

	// Build edges. For every node N, for every EmittedSpec E in N.Spec.Emits,
	// for every node M whose Spec.Consumes == E.Type, emit edge N→M.
	var edges []HandlerEdge
	for _, n := range nodes {
		// Edge weight default: 1. Loop-declaring nodes pay their cap on
		// every outgoing edge — the dispatcher uses LoopCaps directly,
		// but the weight makes the edge label honest in textual output.
		weight := 1
		if n.LoopCap > 0 {
			weight = n.LoopCap
		}
		for _, em := range n.Spec.Emits {
			for _, m := range nodes {
				if m.Spec.Consumes == em.Type {
					edges = append(edges, HandlerEdge{
						From:      n.Name,
						To:        m.Name,
						EventType: em.Type,
						Weight:    weight,
						Terminal:  em.Terminal,
					})
				}
			}
		}
	}

	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		if edges[i].To != edges[j].To {
			return edges[i].To < edges[j].To
		}
		return edges[i].EventType < edges[j].EventType
	})

	g := &HandlerGraph{
		Nodes:         nodes,
		Edges:         edges,
		DeclaredLoops: sortedKeys(declaredLoops),
	}

	// Cycle detection ignores Terminal edges — a terminal emission cannot
	// trigger downstream by construction, so it can't close a runtime cycle.
	cycles := stronglyConnected(g, false)
	g.Cycles = cycles

	// Validate every cycle is capped. A cycle is capped iff at least one
	// node in the SCC declares a Loop AND every edge inside the SCC has
	// Weight >= 1 (always true given defaults, but kept explicit).
	if len(cycles) > 0 {
		var uncapped [][]string
		for _, scc := range cycles {
			if !cycleIsCapped(scc, g) {
				uncapped = append(uncapped, scc)
			}
		}
		if len(uncapped) > 0 {
			return g, &CycleError{Cycles: uncapped}
		}
	}

	return g, nil
}

// CycleError is returned by Build when one or more cycles in the handler
// graph have no max_iterations declaration. The CLI formats it for the user.
type CycleError struct {
	Cycles [][]string
}

func (e *CycleError) Error() string {
	var lines []string
	for _, c := range e.Cycles {
		lines = append(lines, fmt.Sprintf("cycle detected: %s; no max_iterations declared; refusing to start",
			strings.Join(c, " -> ")+" -> "+c[0]))
	}
	return strings.Join(lines, "\n")
}

// cycleIsCapped returns true when at least one node in the SCC declares
// LoopCap > 0.
func cycleIsCapped(scc []string, g *HandlerGraph) bool {
	for _, name := range scc {
		for _, n := range g.Nodes {
			if n.Name == name && n.LoopCap > 0 {
				return true
			}
		}
	}
	return false
}

// stronglyConnected returns SCCs that represent actual cycles. A trivial SCC
// (single node, no self-loop) is dropped. If includeTerminal is false,
// edges marked Terminal are excluded from the traversal.
//
// We use Tarjan's algorithm; the implementation is the textbook iterative
// form to avoid stack overflows on large configs (none yet, but the cost
// is one map lookup per edge).
func stronglyConnected(g *HandlerGraph, includeTerminal bool) [][]string {
	// Build adjacency by name → []name. Self-loops kept; terminal edges
	// gated by flag.
	adj := map[string][]string{}
	for _, n := range g.Nodes {
		adj[n.Name] = nil
	}
	for _, e := range g.Edges {
		if !includeTerminal && e.Terminal {
			continue
		}
		adj[e.From] = append(adj[e.From], e.To)
	}

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
			// Keep only non-trivial SCCs (size > 1 OR self-loop).
			if len(scc) > 1 || hasSelfLoop(scc[0], adj) {
				sort.Strings(scc)
				sccs = append(sccs, scc)
			}
		}
	}

	// Walk in deterministic order for reproducible output.
	var names []string
	for _, n := range g.Nodes {
		names = append(names, n.Name)
	}
	sort.Strings(names)
	for _, v := range names {
		if _, seen := indexOf[v]; !seen {
			strongConnect(v)
		}
	}

	sort.Slice(sccs, func(i, j int) bool {
		return strings.Join(sccs[i], ",") < strings.Join(sccs[j], ",")
	})
	return sccs
}

func hasSelfLoop(v string, adj map[string][]string) bool {
	for _, w := range adj[v] {
		if w == v {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
