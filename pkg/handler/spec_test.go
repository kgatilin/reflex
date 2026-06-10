package handler

import (
	"testing"

	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/projection"
)

func TestEveryBuiltinTypeHasRegisteredSpec(t *testing.T) {
	r := BuiltinRegistry()
	want := []string{
		"llm_stub", "tool_call", "printer", "terminator",
		"unhandled_watcher", "echo",
		"parse_target", "gh_query", "triage_rules",
	}
	for _, typ := range want {
		spec, ok := r.SpecOf(typ)
		if !ok {
			t.Errorf("no spec registered for %q", typ)
			continue
		}
		if spec.Type != typ {
			t.Errorf("spec.Type = %q, want %q", spec.Type, typ)
		}
		if spec.Description == "" {
			t.Errorf("type %q: empty description", typ)
		}
	}
}

func TestEmittersIncludesTerminatorForRequestHandled(t *testing.T) {
	r := BuiltinRegistry()
	em := r.Emitters(projection.TypeRequestHandled)
	if !contains(em, "terminator") {
		t.Errorf("Emitters(RequestHandled) = %v, want includes terminator", em)
	}
	if !contains(em, "llm_stub") {
		t.Errorf("Emitters(RequestHandled) = %v, want includes llm_stub", em)
	}
}

func TestConsumersIncludesDynamicHandlers(t *testing.T) {
	r := BuiltinRegistry()
	c := r.Consumers("SomeWeirdEvent")
	// Dynamic-Consumes handlers (Consumes = "*") should appear for any
	// event type, since at YAML-bind time they could subscribe to anything.
	if !contains(c, "llm_stub") {
		t.Errorf("Consumers should include dynamic llm_stub, got %v", c)
	}
}

func TestResolveSpecSubstitutesEchoEmit(t *testing.T) {
	r := BuiltinRegistry()
	resolved, ok := r.ResolveSpec(config.HandlerConfig{
		Name:   "bounce",
		Type:   "echo",
		On:     "RequestReceived",
		Config: map[string]any{"emit": "Foo"},
	})
	if !ok {
		t.Fatal("ResolveSpec(echo) returned !ok")
	}
	if resolved.Consumes != "RequestReceived" {
		t.Errorf("Consumes = %q, want RequestReceived", resolved.Consumes)
	}
	if len(resolved.Emits) != 1 || resolved.Emits[0].Type != "Foo" {
		t.Errorf("Emits = %+v, want [{Foo,...}]", resolved.Emits)
	}
}

func TestListTypesSortedAndComplete(t *testing.T) {
	r := BuiltinRegistry()
	list := r.ListTypes()
	if len(list) < 9 {
		t.Errorf("ListTypes = %v, want at least 9 entries", list)
	}
	for i := 1; i < len(list); i++ {
		if list[i-1] >= list[i] {
			t.Errorf("ListTypes not sorted: %v", list)
			break
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
