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
//   - sys.{kind}                 → class "sys",          scope "",         kind "{kind}"
//   - app.session.{id}.{kind...} → class "app.session.{id}", scope "session", kind "{kind...}"
//   - {kind...} (anything else)  → class "",             scope "",         kind "{kind...}"
//
// The framework knows only TWO structural classes — sys (its own machinery) and
// app.session (the session scope). Everything else is a plain DOMAIN event: the
// subject IS the kind, with no class prefix and no scope token. The kernel does
// not carve special namespaces for "inbound"/"ingress" events — an externally
// appended event is just a registered domain event whose name the operator
// chose (CONCEPT §2: the framework knows no domain event names). Its
// entry-point-ness is a graph property (consumed by some subscriber, produced by
// none — validate.go isRoot), not a subject-class marker.
//
// The scope token is the qualifier a node's In matches against (§5); only the
// session class carries one. The kind tail is what a node's On patterns match
// (§2). Scope membership for domain cones is causal (caused_by geometry, doc 24
// §5), not derived from the subject string, so a class-less domain subject loses
// nothing the runtime needs.
func splitSubject(subject string) (class, scope, kind string) {
	toks := strings.Split(subject, ".")
	switch {
	case len(toks) == 0:
		return "", "", subject
	case toks[0] == "sys":
		return "sys", "", strings.Join(toks[1:], ".")
	case len(toks) >= 3 && toks[0] == "app" && toks[1] == "session":
		// app.session.{id}.{kind...}: class+scope is app.session.{id}; the
		// scope token a node matches on is "session".
		return "app.session." + toks[2], "session", strings.Join(toks[3:], ".")
	default:
		// A plain domain event: no class prefix, the whole subject is the kind.
		return "", "", subject
	}
}

// KindOf returns an event's kind tail — the part a node's On patterns match
// (doc 24 §2). It is the exported read surface a view-type builder (doc 26 §4b)
// uses to classify matched events without re-deriving the subject grammar.
func KindOf(ev Event) string {
	_, _, kind := splitSubject(ev.Subject)
	return kind
}

// MatchKind reports whether a subscription pattern matches a kind tail — the
// exported matcher a view-type builder uses for role/boundary classification
// (e.g. nodes/llm: "is this event's kind one of my Emits?"). Same grammar as a
// node's On (doc 24 §2).
func MatchKind(pattern, kind string) bool { return subjectMatch(pattern, kind) }

// placeSubject reconstructs a concrete subject from a class+scope prefix and a
// kind tail (§2 uprightness: the dispatcher, not the reaction, places scope).
// class already carries the resolved scope prefix produced by splitSubject
// (e.g. "app.session.s1"), so the subject is simply class + "." + kind. A
// class-less domain event (class == "") places flat: the subject is just the kind.
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
