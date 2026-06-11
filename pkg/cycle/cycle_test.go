package cycle

import (
	"reflect"
	"testing"
)

func TestDetectUncappedCycle_Empty(t *testing.T) {
	cyc, found := DetectUncappedCycle(nil)
	if found {
		t.Fatalf("empty graph: unexpected cycle %v", cyc)
	}
}

func TestDetectUncappedCycle_SingleNoEdges(t *testing.T) {
	// One edge a -> b, no back-edge; not a cycle.
	cyc, found := DetectUncappedCycle([]Edge{
		{From: "a", To: "b"},
	})
	if found {
		t.Fatalf("acyclic 2-node: unexpected cycle %v", cyc)
	}
}

func TestDetectUncappedCycle_SelfLoop(t *testing.T) {
	cyc, found := DetectUncappedCycle([]Edge{
		{From: "a", To: "a"},
	})
	if !found {
		t.Fatalf("self-loop: expected cycle")
	}
	if !reflect.DeepEqual(cyc, []string{"a"}) {
		t.Fatalf("self-loop cycle = %v, want [a]", cyc)
	}
}

func TestDetectUncappedCycle_SelfLoopCapped(t *testing.T) {
	cyc, found := DetectUncappedCycle([]Edge{
		{From: "a", To: "a", Capped: true},
	})
	if found {
		t.Fatalf("capped self-loop should be acceptable, got %v", cyc)
	}
}

func TestDetectUncappedCycle_TwoCycle(t *testing.T) {
	cyc, found := DetectUncappedCycle([]Edge{
		{From: "a", To: "b"},
		{From: "b", To: "a"},
	})
	if !found {
		t.Fatalf("expected 2-cycle to be detected")
	}
	if !reflect.DeepEqual(cyc, []string{"a", "b"}) {
		t.Fatalf("2-cycle = %v, want [a b]", cyc)
	}
}

func TestDetectUncappedCycle_TwoCycleCapped(t *testing.T) {
	// Either edge being Capped should absolve the cycle.
	cyc, found := DetectUncappedCycle([]Edge{
		{From: "a", To: "b", Capped: true},
		{From: "b", To: "a"},
	})
	if found {
		t.Fatalf("capped 2-cycle should pass; got %v", cyc)
	}
}

func TestDetectUncappedCycle_ThreeCycle(t *testing.T) {
	cyc, found := DetectUncappedCycle([]Edge{
		{From: "a", To: "b"},
		{From: "b", To: "c"},
		{From: "c", To: "a"},
	})
	if !found {
		t.Fatalf("expected 3-cycle")
	}
	if !reflect.DeepEqual(cyc, []string{"a", "b", "c"}) {
		t.Fatalf("3-cycle = %v, want [a b c]", cyc)
	}
}

func TestDetectUncappedCycle_MixedCappedAndUncapped(t *testing.T) {
	// Two disjoint 2-cycles: one capped (x↔y), one not (a↔b). The
	// uncapped one must be returned. Lexical order means a/b come first
	// among nodes, but the algorithm doesn't promise which uncapped SCC
	// wins when multiple exist — here there's only one uncapped, so it
	// must be returned regardless.
	cyc, found := DetectUncappedCycle([]Edge{
		{From: "a", To: "b"},
		{From: "b", To: "a"},
		{From: "x", To: "y", Capped: true},
		{From: "y", To: "x"},
	})
	if !found {
		t.Fatalf("expected the uncapped a↔b cycle")
	}
	if !reflect.DeepEqual(cyc, []string{"a", "b"}) {
		t.Fatalf("uncapped cycle = %v, want [a b]", cyc)
	}
}

func TestDetectUncappedCycle_NodeOnlyAppearsAsTarget(t *testing.T) {
	// b only appears as a target. Should not crash and should not invent
	// a cycle.
	cyc, found := DetectUncappedCycle([]Edge{
		{From: "a", To: "b"},
	})
	if found {
		t.Fatalf("unexpected cycle %v on a->b alone", cyc)
	}
}

func TestDetectUncappedCycle_DisjointAcyclicAndCycle(t *testing.T) {
	// Disjoint subgraphs: one acyclic (p->q->r), one a 2-cycle (a↔b).
	cyc, found := DetectUncappedCycle([]Edge{
		{From: "p", To: "q"},
		{From: "q", To: "r"},
		{From: "a", To: "b"},
		{From: "b", To: "a"},
	})
	if !found {
		t.Fatalf("expected to find the a↔b cycle even with disjoint acyclic chain")
	}
	if !reflect.DeepEqual(cyc, []string{"a", "b"}) {
		t.Fatalf("cycle = %v, want [a b]", cyc)
	}
}

func TestDetectUncappedCycle_LongerCycleWithCappedNode(t *testing.T) {
	// 4-cycle a->b->c->d->a, with the cap on d. Should be acceptable.
	cyc, found := DetectUncappedCycle([]Edge{
		{From: "a", To: "b"},
		{From: "b", To: "c"},
		{From: "c", To: "d"},
		{From: "d", To: "a", Capped: true},
	})
	if found {
		t.Fatalf("capped 4-cycle should pass; got %v", cyc)
	}
}
