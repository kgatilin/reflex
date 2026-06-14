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
		{"app.ingress.>", "app.ingress.cli.message", true},
		{"app.ingress.*", "app.ingress.cli", true},
		{"app.ingress.*", "app.ingress.cli.message", false},

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
