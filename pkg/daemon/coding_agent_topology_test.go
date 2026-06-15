package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kgatilin/reflex/pkg/daemon"
	"github.com/kgatilin/reflex/pkg/topology"
)

// TestCodingAgentTopology_AppliesWithRealHands proves the committed coding-agent
// document (topologies/coding-agent.yaml) is a CONNECTED graph once the real fs +
// pytest plugins are launched — the way `reflexd serve --root` runs it. It builds
// reflexd, launches the two hands (which self-register their tool.* kinds), folds
// them with the operator document, and applies: a green Apply means the brain's
// tool calls have consumers (the hands), the hands' results have a consumer (the
// brain), the verifier/terminal/lifecycle paths close, and the llm body resolves
// its Gemini provider (lazily — no model call, no credentials needed at apply).
//
// The one thing this cannot assert is a live model turn: that needs GCP creds,
// network, and spend. The emit-and-drive run is the documented manual step
// (topologies/README.md). This test is the structural proof up to that boundary.
func TestCodingAgentTopology_AppliesWithRealHands(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the reflexd binary and spawns plugins; skipped under -short")
	}
	ctx := context.Background()
	reflexd := buildReflexd(t)
	root := t.TempDir()

	d := daemon.New()
	defer d.Close()

	// The hands, exactly as `serve --root` launches them: confined to the root,
	// self-registering tool.fs.* and tool.py.test.* with their schemas.
	for _, name := range []string{"fs", "pytest"} {
		if _, err := d.LaunchPlugin(ctx, []string{reflexd, "plugin", name, "--root", root}); err != nil {
			t.Fatalf("LaunchPlugin %q: %v", name, err)
		}
	}

	doc := loadTopology(t, filepath.Join("..", "..", "topologies", "coding-agent.yaml"))
	if err := d.Apply(ctx, doc); err != nil {
		t.Fatalf("Apply coding-agent.yaml with fs+pytest launched: %v", err)
	}

	// The hands' kinds landed in the catalog as facts (schemas from the plugins).
	for _, kind := range []string{"tool.fs.read.call", "tool.fs.edit.call", "tool.py.test.call"} {
		if !hasEventRegistered(d.Events(), kind) {
			t.Errorf("no sys.event.registered fact for %q — the hand did not self-register", kind)
		}
	}
	if n := len(d.Topology()); n == 0 {
		t.Errorf("live topology is empty after a successful apply")
	}
}

// TestCodingAgentTopology_Parses proves the document parses and converts to engine
// decls (a fast, no-binary guard that the YAML stays well-formed). It does NOT
// validate connectivity — that needs the launched hands (the test above).
func TestCodingAgentTopology_Parses(t *testing.T) {
	doc := loadTopology(t, filepath.Join("..", "..", "topologies", "coding-agent.yaml"))
	if _, err := doc.Decls(); err != nil {
		t.Fatalf("coding-agent.yaml → decls: %v", err)
	}
	if len(doc.Subscribers) == 0 || len(doc.Scopes) == 0 {
		t.Fatalf("coding-agent.yaml parsed empty (subscribers=%d scopes=%d)", len(doc.Subscribers), len(doc.Scopes))
	}
}

func loadTopology(t *testing.T, path string) topology.Document {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	doc, err := topology.Parse(data)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc
}
