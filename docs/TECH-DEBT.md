# Tech debt

Known shortcuts taken to ship, with the principled fix. Add an entry when you
knowingly leave one; remove it when paid down.

## Tool-kind classification couples to the `tool.*` naming convention

`nodes/llm/llm.go` — `isToolCall` and `toolResultCallName` decide whether a kind
is a function call, a function result, or neither by **string surgery on the
kind**: `HasPrefix("tool.")` + `HasSuffix(".call")` for a call;
`.result`/`.failed` → `.call` for the call a result answers.

```go
func isToolCall(kind string, emits []string) bool {
	return strings.HasPrefix(kind, "tool.") && strings.HasSuffix(kind, ".call") && matchesAny(emits, kind)
}
```

Why it's debt: the builder infers structure from a naming *convention* rather
than from declared facts. A tool whose kinds don't follow `tool.X.{call,result,
failed}` silently won't be encoded as structured function-call parts (it falls
back to text). The call↔result pairing is implicit string manipulation, not a
relationship the catalog records.

Principled fix: a kind should **declare its role** and its call↔result pairing in
the catalog (`engine.EventKind` metadata, sourced from the plugin's
self-description — the plugin already knows which of its kinds are calls vs
results). The builder then reads `kind.role == call` / `result.answers == callKind`
instead of parsing names. This also removes the `tool.` prefix assumption, so
non-`tool.*` request/response pairs (any future plugin) work too.

Introduced: structured llm.history function-call/response reconstruction
(doc 29 Iteration 4, the gemini function-calling fix).
