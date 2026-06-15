package engine

import "testing"

func TestSubjectMatch(t *testing.T) {
	cases := []struct {
		pattern string
		subject string
		want    bool
	}{
		// literal
		{"a.b.c", "a.b.c", true},
		{"a.b.c", "a.b.d", false},
		{"a.b", "a.b.c", false}, // pattern shorter, no tail wildcard
		{"a.b.c", "a.b", false}, // pattern longer

		// single-token wildcard
		{"a.*.c", "a.b.c", true},
		{"a.*.c", "a.x.c", true},
		{"a.*.c", "a.b.d", false},
		{"a.*", "a.b", true},
		{"a.*", "a.b.c", false}, // * is exactly one token
		{"*.b.c", "a.b.c", true},

		// tail wildcard
		{"a.>", "a.b", true},
		{"a.>", "a.b.c.d", true},
		{"a.>", "a", false}, // > needs at least one trailing token
		{">", "a.b.c", true},
		{"cli.>", "cli.task.message", true},
		{"cli.*", "cli.task", true},
		{"cli.*", "cli.task.message", false},

		// mixed
		{"tool.fs.>", "tool.fs.read.call", true},
		{"tool.fs.>", "tool.gotool.build.call", false},
		{"state.updated.plan.>", "state.updated.plan.0.status", true},
		{"state.updated.plan.>", "state.updated.plan", false},

		// empties
		{"", "a.b", false},
		{"a.b", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		if got := subjectMatch(c.pattern, c.subject); got != c.want {
			t.Errorf("subjectMatch(%q, %q) = %v; want %v", c.pattern, c.subject, got, c.want)
		}
	}
}

// TestSplitSubject pins the grammar contract (CONCEPT §2): the framework knows
// only two structural classes — sys and app.session — and EVERYTHING ELSE is a
// plain domain event whose subject IS its kind (no class prefix, no scope
// token). There is no "ingress" class: an externally appended event is just a
// registered domain event.
func TestSplitSubject(t *testing.T) {
	cases := []struct {
		subject               string
		class, scope, kind    string
	}{
		// sys: scope-less machinery.
		{"sys.event.registered", "sys", "", "event.registered"},
		{"sys.topology.changeset.applied", "sys", "", "topology.changeset.applied"},
		// app.session.{id}: the one scoped class.
		{"app.session.s1.state.updated.goal", "app.session.s1", "session", "state.updated.goal"},
		// domain events: the subject IS the kind, class-less, no scope token.
		{"cli.task", "", "", "cli.task"},
		{"request.received", "", "", "request.received"},
		{"tool.fs.read.call", "", "", "tool.fs.read.call"},
		{"scope.request.closed", "", "", "scope.request.closed"},
		// a former "ingress" subject is now just a domain kind, no special split.
		{"app.ingress.cli.task", "", "", "app.ingress.cli.task"},
	}
	for _, c := range cases {
		cls, scope, kind := splitSubject(c.subject)
		if cls != c.class || scope != c.scope || kind != c.kind {
			t.Errorf("splitSubject(%q) = (%q,%q,%q); want (%q,%q,%q)",
				c.subject, cls, scope, kind, c.class, c.scope, c.kind)
		}
	}
}
