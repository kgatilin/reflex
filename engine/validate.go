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

// attrBudgeted tags whether a graph node sits within a budgeted scope; the
// unbounded-cycle check reads it back per node ("true"/"false").
const attrBudgeted = "budgeted"

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

	// UnboundedCycles are non-trivial SCCs (by node name) not covered by a
	// budgeted scope (doc 24 §5 "loops are budgets"): a real cycle with no
	// scope budget to bound the count of a kind within its cone, so nothing
	// forces an exit. Termination is a scope property, not a node property.
	// Each inner slice is one SCC.
	UnboundedCycles [][]string

	// StalledClosures are scope names whose engine-emitted scope.{name}.closed
	// has no consumer (doc 26 §3f / doc 27 §5): a close that can land on a
	// non-terminal state with nothing to read it freezes the cone in the void.
	// Each needs a continuation — an LLM bridge or a deterministic terminator
	// that consumes scope.{name}.closed — or a provably terminal-only close.
	// Sorted ascending.
	StalledClosures []string

	// CoRootedScopes are pairs of distinct scope names that can root on the
	// same event (their root triggers overlap), so one span would open two
	// scope instances — a degenerate co-rooting forbidden by the model (doc 24
	// §5 / doc 26 §3d): a span roots at most one scope. Two scopes rooted on one
	// span share an identical cone (same root, same membership, same obligation
	// count) and add nothing over a single scope carrying both budgets in its
	// Budget map. Each inner slice is the two scope names, sorted; the outer
	// slice is sorted.
	CoRootedScopes [][]string

	// UnknownKinds are kinds in some node's Emits that are absent from the event
	// catalog (doc 26 §4a / 27 §5): a node declares it produces a kind the type
	// layer has never registered, so the kind has no schema and is not
	// advertisable. DORMANT when the catalog is empty (opt-in-until-adopted).
	// Sorted ascending.
	UnknownKinds []string

	// DeadSubscriptions are On patterns that match NO catalog kind (doc 26 §4a /
	// 27 §5): a subscription that can never fire because the type layer carries
	// no kind it could match. DORMANT when the catalog is empty. Sorted
	// ascending by pattern.
	DeadSubscriptions []string

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
	// carrying a "budgeted" marker (is its scope budget-bounded?); a directed
	// edge X→Y iff a kind X emits is matched by a pattern Y subscribes to.
	budgeted := budgetedScopes(decls)
	gvNodes := make([]graphval.Node, 0, len(nodes))
	for _, n := range nodes {
		mark := "false"
		if _, ok := budgeted[n.In]; ok {
			mark = "true"
		}
		gvNodes = append(gvNodes, graphval.Node{
			Name: n.Name,
			Attrs: map[string]string{
				attrBudgeted: mark,
			},
		})
	}

	// emitsByNode is each node's effective produced kinds: its declared Emits
	// plus the scope.{name}.closed kinds it roots (doc 24 §5). The closure kind
	// is engine-emitted, but causally it is produced by the rooting node, so the
	// reachability/cycle graph must carry an edge from the rooter to the
	// closure's consumer — otherwise a legitimate scope.closed consumer (the
	// lifecycle terminator of doc 26 §3f) reads as unreachable.
	emitsByNode := effectiveEmits(decls, nodes)

	var gvEdges []graphval.Edge
	for _, x := range nodes {
		for _, y := range nodes {
			if producesFor(emitsByNode[x.Name], y) {
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
	rep.StalledClosures = stalledClosures(decls, nodes)
	rep.CoRootedScopes = coRootedScopes(decls, nodes)

	// Catalog checks (doc 26 §4a). The catalog is folded from the EventKind decls
	// alone here — Validate is pure over decls, with no log (dynamic
	// event.registered facts are folded at runtime, and a changeset that adds
	// them re-runs Validate against the grown catalog). These two checks are the
	// STATIC type-layer gates; payload-conformance is runtime (engine.go).
	//
	// OPT-IN-UNTIL-ADOPTED (doc 26 §4a, the critical non-breaking rule): the
	// catalog is a type layer a topology grows into. When NO catalog is declared
	// (no EventKind decls, no event.registered facts), it is empty and the two
	// checks are DORMANT — an unadopted catalog must not fail every existing
	// topology. Only a NON-empty catalog gates Connected. The operator may
	// tighten this to "always required" later.
	cat := foldCatalog(decls, nil)
	if !cat.empty() {
		rep.UnknownKinds = unknownKinds(nodes, cat)
		rep.DeadSubscriptions = deadSubscriptions(consumers, cat)
	}

	rep.Suggestions = suggestions(rep)
	rep.Connected = len(rep.DeadEnds) == 0 &&
		len(rep.UnreachableNodes) == 0 &&
		len(rep.Fragments) == 0 &&
		len(rep.UnboundedCycles) == 0 &&
		len(rep.StalledClosures) == 0 &&
		len(rep.CoRootedScopes) == 0 &&
		len(rep.UnknownKinds) == 0 &&
		len(rep.DeadSubscriptions) == 0

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

// producesFor reports whether any kind in produced is matched by some pattern
// in y.On (doc 27 §4): the directed edge X→Y of the topology graph, generalised
// to take X's effective produced kinds (Emits + rooted closures).
func producesFor(produced []string, y Node) bool {
	for _, kind := range produced {
		for _, pat := range y.On {
			if subjectMatch(pat, kind) {
				return true
			}
		}
	}
	return false
}

// effectiveEmits maps each node to its effective produced kinds: declared
// Emits plus the scope.{name}.closed kinds the node roots (doc 24 §5 rooting).
// A node roots a scope two ways: it carries Node.Scope (node-rooted), or it
// emits the declared Root kind of some Scope (kind-rooted) — in either case the
// engine will emit scope.{name}.closed caused by that rooting, so the graph
// edge runs from the rooter to the closure's consumer.
func effectiveEmits(decls []Decl, nodes []Node) map[string][]string {
	declared := declaredScopes(decls)
	out := make(map[string][]string, len(nodes))
	for _, n := range nodes {
		kinds := append([]string(nil), n.Emits...)
		seen := map[string]struct{}{}
		addClosure := func(scopeName string) {
			k := "scope." + scopeName + ".closed"
			if _, ok := seen[k]; ok {
				return
			}
			seen[k] = struct{}{}
			kinds = append(kinds, k)
		}
		if n.Scope != "" {
			addClosure(n.Scope)
		}
		for _, s := range declared {
			if s.Root == "" {
				continue
			}
			for _, k := range n.Emits {
				if subjectMatch(s.Root, k) {
					addClosure(s.Name)
				}
			}
		}
		out[n.Name] = kinds
	}
	return out
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

// isIngressPattern reports whether a subscription pattern is an ingress-root
// pattern: it literally is one of the ingress namespace patterns, or it matches
// a representative ingress subject. Such a pattern subscribes to pre-resolution
// inbound (doc 27 §3) and is never a dead subscription — its producers are
// adapters appending ingress events, not topology nodes / catalog kinds.
func isIngressPattern(pat string) bool {
	for _, ing := range ingressPatterns {
		if pat == ing || subjectMatch(pat, "app.ingress.surface") {
			return true
		}
	}
	return false
}

func isIngressRoot(n Node) bool {
	// A node is an ingress root if any subscription matches the ingress namespace
	// — either by literally carrying an app.ingress.* / .> pattern or by matching
	// a representative ingress subject.
	for _, pat := range n.On {
		if isIngressPattern(pat) {
			return true
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

// unboundedCycles returns the non-trivial SCCs not covered by a budgeted scope
// (doc 24 §5 "loops are budgets"): termination is a scope property, not a node
// property. A cycle terminates because a scope budget bounds the count of a
// kind within its cone; determinism of a node never gave termination (two
// deterministic nodes A: on Y → emit X and B: on X → emit Y ping-pong
// X→Y→X… forever). So the basis is scope-budget coverage, not node kind.
//
// It walks NonTrivialSCCsAsNames and reports each SCC that contains any node
// whose "budgeted" attr is "false" — i.e. a node outside every budgeted scope.
// graphval's SCCsMissingAttr has the wrong orientation here (it returns SCCs
// where NO node satisfies a predicate, an all-fail test; this is an any-fail
// test), so the per-node budgeted lookup is folded reflex-side over the SCCs.
//
// APPROXIMATION (refinement TODO, doc 24 §5): the precise rule is that the
// budget must bound a kind that actually appears on the cycle's edges. Step 1
// approximates that with the coarser "every cycle node is within a budgeted
// scope" — a node in a budgeted scope whose budget bounds an unrelated kind is
// still treated as bounded here. Tightening this to match the cycle's edge
// kinds against the scope's Budget keys is left as future work.
func unboundedCycles(g *graphval.Graph) [][]string {
	var out [][]string
	for _, scc := range g.NonTrivialSCCsAsNames() {
		for _, name := range scc {
			i, ok := g.Index(name)
			if !ok {
				continue
			}
			if g.Attrs(i)[attrBudgeted] != "true" {
				out = append(out, scc)
				break
			}
		}
	}
	return out
}

// stalledClosures returns the scope names whose engine-emitted
// scope.{name}.closed kind has no consumer (doc 26 §3f / doc 27 §5). A scope
// instance can quiesce on a non-terminal state — a stall, mechanically quiet
// but not converged (doc 26 §2) — and the gap between "frozen" and "done" is
// exactly a connectivity dead-end: the closure needs a bridge (an LLM node or
// a deterministic terminator) that reads the frozen cone and either emits a
// terminal event or re-drives into a new child cone.
//
// Scope names come from both rooting sources (doc 24 §5): declared Scope decls
// and node-rooted scopes (Node.Scope). A consumer is any node whose On matches
// scope.{name}.closed.
//
// APPROXIMATION (the safe default of doc 26 §3f): every declared scope's
// scope.{name}.closed must have a consumer, full stop. The doc's stronger rule
// exempts a "provably terminal-only" close — a scope all of whose quiescent
// states are terminal, so no bridge is needed. Proving terminal-only is a
// liveness property over the cone's reachable states, which the static
// subscriber graph does not carry (the engine is payload-blind and does not
// model state values), so this check does not attempt it: it treats every
// closure as potentially stalling and demands a consumer. Tightening to exempt
// provably terminal-only closes is left as future work, mirroring how
// unboundedCycles documents its own coarser approximation.
func stalledClosures(decls []Decl, nodes []Node) []string {
	names := map[string]struct{}{}
	for _, d := range decls {
		if s, ok := d.(Scope); ok && s.Name != "" {
			names[s.Name] = struct{}{}
		}
	}
	for _, n := range nodes {
		if n.Scope != "" {
			names[n.Scope] = struct{}{}
		}
	}

	var out []string
	for name := range names {
		closed := "scope." + name + ".closed"
		consumed := false
		for _, n := range nodes {
			for _, pat := range n.On {
				if subjectMatch(pat, closed) {
					consumed = true
					break
				}
			}
			if consumed {
				break
			}
		}
		if !consumed {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// coRootedScopes returns the pairs of distinct scope names that can root on the
// same event — a span opening two scope instances, forbidden by the model (doc
// 24 §5 / doc 26 §3d). A span roots at most one scope: two scopes rooted on one
// span share an identical cone, so the only legitimate "two budgets over one
// cone" need is served by a single scope with two entries in its Budget map,
// never by co-rooting.
//
// A scope's root triggers come from both rooting sources (doc 24 §5): a declared
// Scope's Root, and a node-rooted scope's On (the node roots when it fires).
// Two scope names collide if any of their triggers can match a common event.
//
// APPROXIMATION: trigger overlap is decided token-wise by patternsOverlap, which
// treats a ">" tail as conservatively overlapping (it may report a collision for
// patterns that share no concrete subject). Erring toward rejection is correct
// for a hard constraint — a false positive is a topology made to name its scopes
// disjointly, never a co-rooting slipping through.
func coRootedScopes(decls []Decl, nodes []Node) [][]string {
	triggers := rootTriggers(decls, nodes)

	names := make([]string, 0, len(triggers))
	for name := range triggers {
		names = append(names, name)
	}
	sort.Strings(names)

	var out [][]string
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if triggersOverlap(triggers[names[i]], triggers[names[j]]) {
				out = append(out, []string{names[i], names[j]})
			}
		}
	}
	return out
}

// rootTriggers maps each scope name to the kind patterns whose dispatch roots an
// instance of it (doc 24 §5): a declared Scope contributes its Root; a
// node-rooted scope contributes the node's On patterns (the node roots on the
// events it fires on). A declared Scope with an empty Root is budget-only
// (rooted by a same-named node) and contributes no trigger.
func rootTriggers(decls []Decl, nodes []Node) map[string][]string {
	out := map[string][]string{}
	for _, d := range decls {
		if s, ok := d.(Scope); ok && s.Name != "" && s.Root != "" {
			out[s.Name] = append(out[s.Name], s.Root)
		}
	}
	for _, n := range nodes {
		if n.Scope != "" {
			out[n.Scope] = append(out[n.Scope], n.On...)
		}
	}
	return out
}

// triggersOverlap reports whether any pattern in a can match a common event with
// any pattern in b.
func triggersOverlap(a, b []string) bool {
	for _, pa := range a {
		for _, pb := range b {
			if patternsOverlap(pa, pb) {
				return true
			}
		}
	}
	return false
}

// patternsOverlap reports whether two subject-kind patterns can match a common
// concrete subject, under the subject grammar (doc 24 §2: "*" one token, ">"
// tail of one or more tokens, a literal token exact). Token-wise: a ">" in
// either pattern makes the remaining tails overlap (conservative — ">" matches
// any non-empty tail); a "*" matches any single token; two literals must be
// equal; and patterns that run out at different lengths (without a ">") cannot
// share a subject.
func patternsOverlap(a, b string) bool {
	ta, tb := splitTokens(a), splitTokens(b)
	for i := 0; ; i++ {
		aEnd, bEnd := i >= len(ta), i >= len(tb)
		if aEnd && bEnd {
			return true // same length, every token compatible
		}
		if aEnd || bEnd {
			return false // different length, no ">" reached — no common subject
		}
		x, y := ta[i], tb[i]
		if x == ">" || y == ">" {
			return true // tail matches the rest (≥1 token) on both sides
		}
		if x == "*" || y == "*" {
			continue // one token each, compatible
		}
		if x != y {
			return false
		}
	}
}

// unknownKinds returns the kinds a node declares in Emits that are absent from
// the event catalog (doc 26 §4a / 27 §5). A kind not in the catalog has no
// schema and is not advertisable — the node claims to produce a type the
// vocabulary has never registered. The engine-emitted scope.{name}.closed
// kinds are NOT in Emits (they are produced by the engine, not declared), so
// this check is exactly "declared Emits vs catalog". Called only over a
// non-empty catalog (opt-in dormancy is the caller's gate). Sorted ascending,
// de-duplicated across nodes.
func unknownKinds(nodes []Node, cat catalog) []string {
	seen := map[string]struct{}{}
	for _, n := range nodes {
		for _, k := range n.Emits {
			if cat.has(k) {
				continue
			}
			seen[k] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// deadSubscriptions returns the On patterns (across nodes and projections) that
// match NO catalog kind (doc 26 §4a / 27 §5): a subscription that can never
// fire because the type layer carries no kind it could match. A pattern is a
// family matcher (subjectMatch over the catalog's concrete kinds); if no
// catalog kind matches, the subscription is dead. Engine-emitted lifecycle
// kinds (scope.{name}.closed) are not catalog entries unless registered, so a
// scope.X.closed subscription is dead under a catalog that omits it — which is
// correct: a topology that adopts the catalog should register the closure kinds
// it consumes. Called only over a non-empty catalog. Sorted ascending,
// de-duplicated across consumers.
func deadSubscriptions(consumers []consumer, cat catalog) []string {
	kinds := cat.kinds()
	seen := map[string]struct{}{}
	for _, c := range consumers {
		for _, pat := range c.on {
			// An ingress-root pattern (app.ingress.*) is NOT dead: it matches
			// externally-appended ingress events (pre-resolution inbound, doc 27
			// §3), which are not produced catalog kinds — there is no internal
			// producer, by design. The reachability check already treats these as
			// entry points (ingressPatterns / isIngressRoot); the catalog check
			// must agree, else adopting a catalog would flag every entry point.
			if isIngressPattern(pat) {
				continue
			}
			matched := false
			for _, k := range kinds {
				if subjectMatch(pat, k) {
					matched = true
					break
				}
			}
			if !matched {
				seen[pat] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// budgetedScopes collects the names of declared Scopes carrying a non-empty
// Budget (doc 24 §5): the scopes that actually bound a per-kind count within a
// cone. A node is "bounded" iff its (defaulted) In names one of these.
func budgetedScopes(decls []Decl) map[string]struct{} {
	out := map[string]struct{}{}
	for _, d := range decls {
		if s, ok := d.(Scope); ok && len(s.Budget) > 0 {
			out[s.Name] = struct{}{}
		}
	}
	return out
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
			"cycle %v is unbounded — declare a budgeted scope covering its nodes so a budget bounds the loop (doc 24 §5)",
			scc))
	}
	for _, name := range rep.StalledClosures {
		out = append(out, fmt.Sprintf(
			"scope %q can close on a non-terminal state with no consumer of \"scope.%s.closed\" — add an llm bridge (or a deterministic terminator) that consumes \"scope.%s.closed\" and emits a terminal event or re-drives into a new child cone (doc 26 §3f)",
			name, name, name))
	}
	for _, pair := range rep.CoRootedScopes {
		out = append(out, fmt.Sprintf(
			"scopes %q and %q can root on the same event — a span roots at most one scope; merge them into one scope and put both budgets in its Budget map (doc 24 §5)",
			pair[0], pair[1]))
	}
	for _, k := range rep.UnknownKinds {
		out = append(out, fmt.Sprintf(
			"kind %q is emitted but not in the event catalog — register it (an EventKind decl, or emit an \"event.registered\" fact carrying {kind: %q, schema}) or fix the name (doc 26 §4a)",
			k, k))
	}
	for _, pat := range rep.DeadSubscriptions {
		out = append(out, fmt.Sprintf(
			"subscription pattern %q matches no catalog kind — it can never fire; fix the pattern or register a producer kind it matches (an EventKind / \"event.registered\", doc 26 §4a)",
			pat))
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
