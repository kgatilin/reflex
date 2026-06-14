package engine

import (
	"context"
	"testing"
)

// fakeHistory is a stand-in custom view type registered by a test, mirroring
// what nodes/llm registers as "llm.history" (doc 26 §4b): the builder is a pure
// function of the matched events, the body reads it type-safely via ViewAs.
type fakeHistory interface {
	Count() int
}

type fakeHistoryImpl struct{ n int }

func (f fakeHistoryImpl) Count() int { return f.n }

func init() {
	RegisterType("test.history", func(_ Projection, events []Event) any {
		return fakeHistoryImpl{n: len(events)}
	})
}

// TestViewType_CustomBuilderResolvesThroughViewAs proves a registered non-builtin
// Type resolves through Views.Value and the generic ViewAs helper, and that the
// builder receives exactly the projection's matched events (doc 26 §4b).
func TestViewType_CustomBuilderResolvesThroughViewAs(t *testing.T) {
	decls := []Decl{
		Scope{Name: "request", Root: "request.received"},
		Node{Name: "resolver", On: []string{"app.ingress.*"}, In: "global", Emits: []string{"request.received"}, Scope: "request"},
		// A reader node whose body asserts the custom view through ViewAs.
		Node{
			Name:  "reader",
			On:    []string{"request.received"},
			In:    "request",
			Reads: []string{"hist"},
			Emits: []string{"reader.saw"},
			Body: ReactionFunc(func(_ context.Context, _ Event, views Views) ([]Emit, error) {
				h := ViewAs[fakeHistory](views, "hist")
				if h == nil {
					t.Errorf("ViewAs returned nil for a registered type")
					return nil, nil
				}
				// Also confirm an unknown name yields the zero value, not a panic.
				if miss := ViewAs[fakeHistory](views, "nope"); miss != nil {
					t.Errorf("ViewAs on unknown name = %v, want nil", miss)
				}
				return nil, nil
			}),
		},
		Node{Name: "sink", On: []string{"reader.saw"}, In: "request"},
		Projection{Name: "hist", On: []string{"request.received"}, In: HorizonRequest, Type: "test.history"},
	}

	ctx := context.Background()
	e := New()
	e.install(decls...)
	if _, err := e.Append(ctx, "app.ingress.cli.task", []byte(`{}`)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := e.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
}

// TestViewType_UnknownTypeRejected proves the validator flags a projection whose
// Type has no registered builder (doc 26 §4b).
func TestViewType_UnknownTypeRejected(t *testing.T) {
	rep, err := Validate(
		Projection{Name: "bad", On: []string{"x"}, In: HorizonRequest, Type: "no.such.type"},
	)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(rep.UnknownViewTypes) != 1 || rep.UnknownViewTypes[0] != "bad" {
		t.Errorf("UnknownViewTypes = %v, want [bad]", rep.UnknownViewTypes)
	}
	if rep.Connected {
		t.Errorf("Connected = true, want false (unknown view type)")
	}
}

// TestViewType_DanglingReadRejected proves the validator flags a node reading a
// view that no projection declares (doc 26 §4b).
func TestViewType_DanglingReadRejected(t *testing.T) {
	rep, err := Validate(
		Node{Name: "n", On: []string{"x"}, In: "global", Reads: []string{"ghost"}},
	)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(rep.DanglingReads) != 1 || rep.DanglingReads[0] != "n:ghost" {
		t.Errorf("DanglingReads = %v, want [n:ghost]", rep.DanglingReads)
	}
}
