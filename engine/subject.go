package engine

import "strings"

// subjectMatch reports whether a subscription pattern matches a concrete
// subject. Subjects are hierarchical, dot-separated token sequences (the
// subject grammar of doc 24 §2: {class}.{scope...}.{kind...}); a pattern is
// the same grammar with two wildcards:
//
//   - "*" matches exactly one token at that position;
//   - ">" matches the remaining tail — one or more trailing tokens — and is
//     only meaningful as the final token of the pattern.
//
// A literal token matches itself only. The pattern and subject must agree
// token-for-token; a pattern with more or fewer tokens than the subject does
// not match unless a ">" absorbs the difference. Empty patterns and empty
// subjects never match.
//
// This is the domain matcher, defined on reflex's own terms. A transport may
// later carry these subjects, but the grammar here is the kernel's, owned by
// the kernel — no transport vocabulary leaks into the name or the rules.
func subjectMatch(pattern, subject string) bool {
	if pattern == "" || subject == "" {
		return false
	}
	p := strings.Split(pattern, ".")
	s := strings.Split(subject, ".")
	for i, tok := range p {
		switch tok {
		case ">":
			// Tail wildcard: matches one or more remaining subject tokens.
			// Only valid as the last pattern token; anything after it is
			// unreachable, so we treat ">" as terminal and require at least
			// one subject token left to absorb.
			return i < len(s)
		case "*":
			// Single-token wildcard: a subject token must exist here.
			if i >= len(s) {
				return false
			}
		default:
			if i >= len(s) || s[i] != tok {
				return false
			}
		}
	}
	// Pattern exhausted with no tail wildcard: subject must be exhausted too.
	return len(s) == len(p)
}

// splitSubject decomposes a concrete subject into its three axes per the §2
// grammar {class}.{scope...}.{kind...}, by class:
//
//   - sys.{kind}                       → class "sys",        scope "",         kind "{kind}"
//   - app.session.{id}.{kind...}       → class "app.session",scope "session",  kind "{kind...}"
//   - app.ingress.{surface}.{event...} → class "app.ingress",scope "",         kind "{surface}.{event...}"
//
// The scope token is the qualifier a node's In matches against (§5); only the
// session class carries one in 2a. The kind tail is what a node's On patterns
// match after handler desugar (§2). Unknown shapes fall back to class = first
// token, no scope, kind = the rest — best-effort so dispatch never panics.
//
// 2a note: the session id itself is not surfaced here — scope matching uses the
// class-level token "session", not the instance id; per-instance cones are 2b.
func splitSubject(subject string) (class, scope, kind string) {
	toks := strings.Split(subject, ".")
	switch {
	case len(toks) >= 1 && toks[0] == "sys":
		return "sys", "", strings.Join(toks[1:], ".")
	case len(toks) >= 3 && toks[0] == "app" && toks[1] == "session":
		// app.session.{id}.{kind...}: class+scope is app.session.{id}; the
		// scope token a node matches on is "session".
		return "app.session." + toks[2], "session", strings.Join(toks[3:], ".")
	case len(toks) >= 2 && toks[0] == "app" && toks[1] == "ingress":
		// app.ingress.{surface}.{event...}: pre-resolution, no session scope;
		// the kind is the surface+event tail.
		return "app.ingress", "", strings.Join(toks[2:], ".")
	default:
		if len(toks) == 0 {
			return "", "", subject
		}
		return toks[0], "", strings.Join(toks[1:], ".")
	}
}

// placeSubject reconstructs a concrete subject from a class+scope prefix and a
// kind tail (§2 uprightness: the dispatcher, not the reaction, places scope).
// class already carries the resolved scope prefix produced by splitSubject
// (e.g. "app.session.s1"), so the subject is simply class + "." + kind. A
// scope-less class (sys, app.ingress) joins the same way.
func placeSubject(class, kind string) string {
	if class == "" {
		return kind
	}
	if kind == "" {
		return class
	}
	return class + "." + kind
}

// sessionOf returns the session id carried by a subject, or "" if the subject
// is not session-scoped. Only the app.session.{id} class carries one.
func sessionOf(subject string) string {
	toks := strings.Split(subject, ".")
	if len(toks) >= 3 && toks[0] == "app" && toks[1] == "session" {
		return toks[2]
	}
	return ""
}
