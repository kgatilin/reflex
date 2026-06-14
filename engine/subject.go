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
