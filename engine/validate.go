package engine

import (
	"fmt"
	"sort"

	graphval "github.com/kgatilin/archmotif/pkg/graphval"
)

// ingressPatterns are the subscription patterns that mark a node as an
// ingress root (doc 27 §3): the resolver subscribes to app.ingress.* to turn
// pre-resolution inbound into request.received. A node whose On contains any
// pattern matching an ingress kind roots the reachable set; it has no
// upstream producer inside the topology and is correct to be a source.
var ingressPatterns = []string{
	"app.ingress.*",
	"app.ingress.>",
}

// nodeKind tags the kind attribute carried on graph nodes; the SCC-guard
// predicate reads it back.
const attrKind = "kind"

// Report is the connectivity validator's structured output (doc 27 §5). It is
// the read-only result of folding a subscriber list into a graph and checking
// it: either Connected, or a set of gaps with human-readable bridge
// suggestions. No event is appended to produce it.
type Report struct {
	// Connected is true iff there are no dead-ends, no unreachable nodes, no
	// disconnected fragments, and no unbounded cycles.
	Connected bool

	// DeadEnds are kinds emitted by some node that no node consumes (doc 27
	// §5): an unbridged gap. Sorted ascending.
	DeadEnds []string

	// UnreachableNodes are nodes with no path from any ingress root and which
	// are not themselves ingress roots. Sorted ascending.
	UnreachableNodes []string

	// Fragments is the complement of the reachable-from-ingress node set:
	// island nodes with no path from an entry point. (Same membership as
	// UnreachableNodes; reported separately because §5 names both the gap —
	// "unreachable node" — and its grouping — "disconnected fragment".)
	Fragments []string

	// UnboundedCycles are non-trivial SCCs (by node name) that contain no
	// deterministic guard node (doc 26 §3a): a real cycle with nothing in it
	// to force an exit. Each inner slice is one SCC.
	UnboundedCycles [][]string

	// Suggestions are human-readable bridge proposals (doc 27 §1/§5), e.g.
	// "kind X is a dead-end — add an llm node that consumes X …".
	Suggestions []string
}

// Validate folds the declarations into a live table and runs the doc 27 §5
// connectivity checks over the resulting topology graph, returning a
// structured Report. It is the read-only changeset validator (doc 20 / doc 24
// §7) in dry-run mode: nothing is appended, no drain runs, the caller's decls
// are not mutated. This is the cleaner of the two entry points — Apply routes
// through it — because validation is a pure function of the decls and the
// Report is the whole product of step 1.
func Validate(decls ...Decl) (Report, error) {
	nodes := foldNodes(decls)
	consumers := foldConsumers(decls, nodes)

	// Build the node graph (doc 27 §4): one graphval node per reaction node,
	// carrying its kind and an ingress-root marker; a directed edge X→Y iff a
	// kind X emits is matched by a pattern Y subscribes to.
	gvNodes := make([]graphval.Node, 0, len(nodes))
	for _, n := range nodes {
		gvNodes = append(gvNodes, graphval.Node{
			Name: n.Name,
			Attrs: map[string]string{
				attrKind: string(n.Kind.orDefault()),
			},
		})
	}

	var gvEdges []graphval.Edge
	for _, x := range nodes {
		for _, y := range nodes {
			if edgeBetween(x, y) {
				gvEdges = append(gvEdges, graphval.Edge{From: x.Name, To: y.Name})
			}
		}
	}

	g, err := graphval.New(gvNodes, gvEdges)
	if err != nil {
		return Report{}, fmt.Errorf("engine: build topology graph: %w", err)
	}

	roots := ingressRoots(nodes)
	rep := Report{}

	rep.DeadEnds, err = deadEnds(nodes, consumers)
	if err != nil {
		return Report{}, err
	}
	rep.UnreachableNodes = unreachable(g, nodes, roots)
	rep.Fragments = append([]string(nil), rep.UnreachableNodes...)
	rep.UnboundedCycles = unboundedCycles(g)

	rep.Suggestions = suggestions(rep)
	rep.Connected = len(rep.DeadEnds) == 0 &&
		len(rep.UnreachableNodes) == 0 &&
		len(rep.Fragments) == 0 &&
		len(rep.UnboundedCycles) == 0

	// allowlistLint is a runtime check, not a static one: it compares an
	// observed emit against the node's declared Emits and can only be made on
	// the log, not on the topology. Left as a stub (see allowlistLint) — the
	// static validator deliberately does not implement it.
	_ = allowlistLint

	return rep, nil
}

// consumer is anything that consumes kinds by subscription: a reaction node
// (its On) or a projection (its On folds kinds into a view, doc 19 / 27 §3).
// The dead-end check treats both as consumers — a state.updated fact folded
// into a view is consumed, not a dead-end — while only nodes are graph
// vertices for reachability and cycles.
type consumer struct {
	name string
	on   []string
}

// foldConsumers gathers every kind-consuming declaration: the reaction nodes
// (already scope-defaulted) plus the projections. Projection names are
// prefixed to keep them distinct from node names in the bipartite relation.
func foldConsumers(decls []Decl, nodes []Node) []consumer {
	out := make([]consumer, 0, len(decls))
	for _, n := range nodes {
		out = append(out, consumer{name: n.Name, on: n.On})
	}
	for _, d := range decls {
		if p, ok := d.(Projection); ok {
			out = append(out, consumer{name: "projection:" + p.Name, on: p.On})
		}
	}
	return out
}

// foldNodes extracts the reaction nodes from the decls and applies scope
// defaulting (doc 27 §4: no node is scope-less; empty In becomes "global").
// It copies each Node so the caller's decls are never mutated in place.
func foldNodes(decls []Decl) []Node {
	var out []Node
	for _, d := range decls {
		n, ok := d.(Node)
		if !ok {
			continue
		}
		if n.In == "" {
			n.In = "global"
		}
		out = append(out, n)
	}
	return out
}

// edgeBetween reports whether x produces a kind that y consumes: some kind in
// x.Emits is matched by some pattern in y.On (doc 27 §4).
func edgeBetween(x, y Node) bool {
	for _, kind := range x.Emits {
		for _, pat := range y.On {
			if subjectMatch(pat, kind) {
				return true
			}
		}
	}
	return false
}

// ingressRoots returns the names of nodes that subscribe to an ingress kind
// (doc 27 §3): the topology's entry points.
func ingressRoots(nodes []Node) []string {
	var out []string
	for _, n := range nodes {
		if isIngressRoot(n) {
			out = append(out, n.Name)
		}
	}
	sort.Strings(out)
	return out
}

func isIngressRoot(n Node) bool {
	for _, pat := range n.On {
		for _, ing := range ingressPatterns {
			// A node is an ingress root if its subscription matches the
			// ingress namespace — either by literally subscribing under
			// app.ingress (pat matches a representative ingress subject) or
			// by carrying an app.ingress.* / .> pattern itself.
			if pat == ing || subjectMatch(pat, "app.ingress.surface") {
				return true
			}
		}
	}
	return false
}

// deadEnds models the bipartite "is this emitted kind consumed?" relation and
// returns the kinds with zero consumers (doc 27 §5). Producers are the
// consumers (nodes + projections); items are every emitted kind; an edge
// consumer→kind means the consumer's On matches the kind. graphval.DeadItems
// surfaces the items (kinds) with no incoming edge — the dead-ends. A kind is
// terminal-and-fine only if it truly has a consumer (e.g. notify consuming
// task.answered); otherwise it is reported.
func deadEnds(nodes []Node, consumers []consumer) ([]string, error) {
	kindSet := map[string]struct{}{}
	for _, n := range nodes {
		for _, k := range n.Emits {
			kindSet[k] = struct{}{}
		}
	}
	items := make([]string, 0, len(kindSet))
	for k := range kindSet {
		items = append(items, k)
	}
	sort.Strings(items)

	producers := make([]string, 0, len(consumers))
	for _, c := range consumers {
		producers = append(producers, c.name)
	}

	var edges []graphval.Edge
	for _, c := range consumers {
		for k := range kindSet {
			for _, pat := range c.on {
				if subjectMatch(pat, k) {
					edges = append(edges, graphval.Edge{From: c.name, To: k})
					break
				}
			}
		}
	}

	dead, err := graphval.DeadItems(producers, items, edges)
	if err != nil {
		return nil, fmt.Errorf("engine: dead-end check: %w", err)
	}
	return dead, nil
}

// unreachable returns the nodes that are neither an ingress root nor reachable
// from one (doc 27 §5). ReachableFromNames(roots) includes each root and
// everything downstream; the complement is the unreachable set.
func unreachable(g *graphval.Graph, nodes []Node, roots []string) []string {
	reachable := map[string]struct{}{}
	for _, name := range g.ReachableFromNames(roots) {
		reachable[name] = struct{}{}
	}
	var out []string
	for _, n := range nodes {
		if _, ok := reachable[n.Name]; !ok {
			out = append(out, n.Name)
		}
	}
	sort.Strings(out)
	return out
}

// unboundedCycles returns the non-trivial SCCs that lack a deterministic guard
// (doc 27 §5, doc 26 §3a). It intersects NonTrivialSCCsAsNames with
// SCCsMissingAttrAsNames(pred) where pred = "the SCC contains a node with
// kind == deterministic"; SCCsMissingAttr already returns the non-trivial SCCs
// failing the predicate, so it IS the intersection.
//
// TODO(doc 26 §3a): this enforces only the "a deterministic guard is present"
// half of the guard check. The "monotone exit" half — that the guard actually
// drives a budget/counter monotonically toward an exit edge — is not yet
// enforced; a deterministic node in the cycle is currently accepted on faith.
func unboundedCycles(g *graphval.Graph) [][]string {
	pred := func(attrs map[string]string) bool {
		return attrs[attrKind] == string(KindDeterministic)
	}
	return g.SCCsMissingAttrAsNames(pred)
}

// suggestions renders human-readable bridge proposals for the gaps (doc 27
// §1/§5). Dead-ends get an LLM-bridge suggestion; unreachable nodes and
// unbounded cycles get explanatory lines.
func suggestions(rep Report) []string {
	var out []string
	for _, k := range rep.DeadEnds {
		out = append(out, fmt.Sprintf(
			"kind %q is a dead-end — add an llm node that consumes %q and emits the entry kinds of the disconnected fragment",
			k, k))
	}
	for _, n := range rep.UnreachableNodes {
		out = append(out, fmt.Sprintf(
			"node %q is unreachable — no path from an ingress root; wire a producer of one of its On kinds, or make it an ingress root",
			n))
	}
	for _, scc := range rep.UnboundedCycles {
		out = append(out, fmt.Sprintf(
			"cycle %v is unbounded — add a deterministic guard node inside it with a monotone exit (doc 26 §3a)",
			scc))
	}
	return out
}

// allowlistLint is the runtime allowlist check (doc 24 §A.4): a node emitting
// a kind outside its declared Emits. It is intentionally a stub — this check
// is observed-emit-vs-declared and can only run against the log at dispatch
// time, not against the static topology. The static validator does not call
// it; it lives here to mark the seam.
func allowlistLint(_ []Node) []string {
	// Runtime concern (doc 27 §5): compare an observed Emit on the log against
	// the emitting node's declared Emits. Not implementable statically.
	return nil
}
