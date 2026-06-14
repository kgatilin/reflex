package topology_test

import (
	"testing"

	"github.com/kgatilin/reflex/engine"
	"github.com/kgatilin/reflex/pkg/topology"
)

const sampleYAML = `
scopes:
  - name: request
    root: request.received
    budget:
      tool.fs.read.call: 16
subscribers:
  - name: brain
    on: [request.received]
    in: request
    reads: [brain.history]
    emits: [task.answered, llm.usage]
    body:
      kind: llm
      config:
        model: vertex:google/gemini-2.5-pro
        system: You are the brain.
  - name: notify
    on: [task.answered]
    in: request
projections:
  - name: brain.history
    on: [request.received, llm.message]
    in: request
    type: llm.history
    params:
      system: You are the brain.
events:
  - kind: task.answered
    schema:
      type: object
`

// TestParseAndDecls proves a YAML document decodes into the four engine decl
// kinds with the body descriptor, horizon, type, and opaque params/schema
// carried through.
func TestParseAndDecls(t *testing.T) {
	doc, err := topology.Parse([]byte(sampleYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	decls, err := doc.Decls()
	if err != nil {
		t.Fatalf("Decls: %v", err)
	}

	var nNodes, nScopes, nProj, nEvents int
	for _, d := range decls {
		switch v := d.(type) {
		case engine.Subscriber:
			nNodes++
			if v.Name == "brain" {
				if v.BodyKind != "llm" {
					t.Errorf("brain.BodyKind = %q, want llm", v.BodyKind)
				}
				if len(v.BodyConfig) == 0 {
					t.Errorf("brain.BodyConfig empty — config not carried through")
				}
			}
		case engine.Scope:
			nScopes++
			if v.Name == "request" && v.Budget["tool.fs.read.call"] != 16 {
				t.Errorf("request budget = %v, want tool.fs.read.call:16", v.Budget)
			}
		case engine.Projection:
			nProj++
			if v.Type != "llm.history" || v.In != engine.HorizonRequest {
				t.Errorf("projection = type %q in %q, want llm.history/request", v.Type, v.In)
			}
		case engine.EventKind:
			nEvents++
		}
	}
	if nNodes != 2 || nScopes != 1 || nProj != 1 || nEvents != 1 {
		t.Errorf("decl counts = %d nodes, %d scopes, %d proj, %d events; want 2/1/1/1", nNodes, nScopes, nProj, nEvents)
	}
}

// TestFromDeclsRoundTrip proves the read side renders live decls back to a
// document that re-decodes to the same shape (the basis of `topo show`).
func TestFromDeclsRoundTrip(t *testing.T) {
	doc, _ := topology.Parse([]byte(sampleYAML))
	decls, _ := doc.Decls()

	rendered := topology.FromDecls(decls)
	back, err := rendered.Decls()
	if err != nil {
		t.Fatalf("re-Decls after FromDecls: %v", err)
	}
	if len(back) != len(decls) {
		t.Fatalf("round-trip decl count = %d, want %d", len(back), len(decls))
	}
}
