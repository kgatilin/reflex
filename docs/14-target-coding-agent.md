# 14 — Target: a minimal coding agent from plugins

The first concrete target the runtime must carry end to end: a coding agent
that makes small, real code changes. It is the two-primitive model of
[15-primitive-reduction.md](./15-primitive-reduction.md) (`llm` + `tool`)
with file tools instead of `calc`, the scope machinery of
[16-engine-architecture.md](./16-engine-architecture.md) (node-rooted
scopes, budgets), the subject/trace envelope of
[13-event-taxonomy.md](./13-event-taxonomy.md), and the projection
interface of [19-projections.md](./19-projections.md) carrying the
read-before-edit guard. Every tool lives as an **out-of-process plugin**
registered over the existing SDK Remote transport. This document defines
the use case, the tool signatures, the scope confinement, and the deltas
from current code. It is a target, not a phase — it names what "done"
looks like.

## The use case

One request, one small code change, fully auditable:

> "In `pkg/foo/bar.go`, the function `Parse` returns `nil` on an empty input;
> make it return an error instead."

The agent must be able to: **find** where the symbol lives, **read** the file,
**edit** it surgically, **format** and **lint** the result, and answer — never
touching anything outside the working directory. That is the whole minimal
scope. No bash, no permission prompts, no multi-file refactors.

A live trace — the loop is a chain of node-rooted scope instances
(doc 15/16); "turn" is not a vocabulary word, the log shows firings rooting
scopes:

```
app.session.S.request.received{ text: "...make Parse return an error..." } ── scope «request» opens
 └─ brain firing 1 ─ tool.fs.search.call{ query: "func Parse" } ─────────── brain scope S₁
      └─ search ─ tool.fs.search.result{ matches: [{ path: "pkg/foo/bar.go", line: 12 }] }
    scope.brain.closed{S₁}
 └─ brain firing 2 ─ tool.fs.read.call{ path: "pkg/foo/bar.go" } ────────── S₂
      └─ read ─ tool.fs.read.result{ content: "...", sha: "ab12", lines: { from: 1, to: 40, total: 40 } }
    scope.brain.closed{S₂}
 └─ brain firing 3 ─ tool.fs.edit.call{ path, old: "return nil", new: "return errEmpty" } ─ S₃
      └─ edit ─ tool.fs.edit.result{ sha: "cd34", replaced: 1 }
    scope.brain.closed{S₃}
 └─ brain firing 4 ─ tool.fmt.run.call{ path: "pkg/foo/bar.go" } ────────── S₄
      └─ fmt ─ tool.fmt.run.result{ changed: ["pkg/foo/bar.go"] }
    scope.brain.closed{S₄}
 └─ brain firing 5 ─ tool.lint.run.call{ path: "pkg/foo/bar.go" } ───────── S₅
      └─ lint ─ tool.lint.run.result{ ok: true, findings: [] }
    scope.brain.closed{S₅}
 └─ brain firing 6 ─ assistant.message(T){ "Parse now returns errEmpty on empty input." }
                   + request.handled(T)
(request cone quiesces) → scope.request.closed — end of the OTel trace
```

One tool per firing is the model being cautious, not the runtime: a firing
may emit several calls at once and the brain scope holds the barrier until
all of them settle — N=1 is the degenerate fan-out (doc 15), so sequential
and parallel are one mechanism.

Whether the change was verified (lint `ok` before the final message) is a
fold over the request cone — the auditability win of 12-react-experiment.md
W4, for free.

## Tools are out-of-process plugins

The mechanism already exists: `reflex daemon` exposes a Unix socket; an
external binary connects via `sdk.Connect(sdk.Remote(socket))`, registers a
handler, and runs in its own process (see `cmd/reflex-sample-handler`). The
bus stays pure; tools run **alongside** it, never inside it.

Plugins live in a dedicated workspace root — `plugins/` — out of the runtime
core, one binary per tool family (or one multi-tool binary):

```
plugins/
  fs/    tool.fs.read / tool.fs.write / tool.fs.edit / tool.fs.search
  fmt/   tool.fmt.run
  lint/  tool.lint.run
```

Each is the sample-handler shape, subscribing to a tool kind:

```go
h := sdk.NewHandler("fs",
    sdk.Consumes("tool.fs.read.call"),     // desugars to app.session.*.tool.fs.read.call
    sdk.Emits("tool.fs.read.result", "tool.fs.read.failed"),
).OnEvent(fsRead)
```

Registering the `fs` plugin teaches the brain the file tools with **zero
config**: the LLM tool menu is a projection of the consumers of
`tool.*.call` (13-event-taxonomy.md / 11-domain-model.md). Add a plugin, the
model gains a tool. The same `hello` registers the plugin's companion
projection — `fs.seen` (below, per doc 19) — so the read-before-edit guard
ships with the tool that enforces it, zero engine changes.

## Tool signatures

Tools carry **structured** payloads — the current `{args: string}` shape
(`pkg/handler/tool_node.go`) is insufficient for files and is the first delta.
Each tool does exactly one thing; none parse shell.

### File tools (`fs`)

| Kind | Call payload | Result payload | Fails when |
|---|---|---|---|
| `tool.fs.read` | `{ path, offset?, limit? }` | `{ path, content, sha, lines: { from, to, total }, truncated }` | missing, outside scope |
| `tool.fs.edit` | `{ path, old, new, replace_all? }` | `{ path, sha, replaced }` | `old` absent / non-unique, no prior read in context, stale read |
| `tool.fs.write` | `{ path, content }` | `{ path, sha }` | outside scope |
| `tool.fs.search` | `{ query, glob?, regex?, max? }` | `{ matches: [{ path, line, text }], truncated }` | — |

The three file tools are a deliberate division of labour — **read a window,
edit in place, write only to create**:

- `read` returns a **window**, not the whole file: `offset`/`limit` (by line,
  with sane defaults) so the model pulls only the slice it needs. Content is
  **line-numbered**, and `lines.total` + `truncated` tell the model there is
  more — never a silent cut.
- `edit` is the **update primitive** and the primary code-change tool: an
  **exact** `old`→`new` replacement, in place, never a rewrite. `old` must be
  **unique** or the call fails (no guessing); `replace_all` is the explicit
  opt-in for a deliberate multi-site change. `replaced` reports the count.
- `write` is create / full overwrite **only** — for new files. It is *not* the
  way to change an existing file; to update, `edit`. (A `write` over an
  existing path is allowed but is the wholesale-rewrite escape hatch, not the
  default path.)
- `sha` is the content hash every fs result reports. It is what the
  `fs.seen` projection folds — the carrier of the read-before-edit guard
  below — and the optimistic-concurrency check once sessions run in
  parallel; the model never threads it by hand.
- `search` is the minimal "find where X is": literal by default, opt-in
  `regex`, optional `glob` filter, `max`-capped with a `truncated` flag (no
  silent truncation).

### Read-before-edit: a projection guard, not a lock

The invariant: an edit is valid only if **the context in which the model
emitted it contained the file's current version**. Doc 19's machinery
carries it in two halves. The `fs` plugin registers a kv projection and
declares it on its edit subscription:

```yaml
projections:
  - name: fs.seen
    on:    [tool.fs.read.result, tool.fs.edit.result, tool.fs.write.result]
    in:    session
    shape: kv
    key:   payload.path
    value: payload.sha

# the plugin's edit handler:
- name: fs
  on:    tool.fs.edit.call
  reads: [fs.seen]
```

The guard, inside the plugin:

```
seen := views["fs.seen"][call.path]
case seen missing        → tool.fs.edit.failed{ "no prior read" }
case seen != sha(disk)   → tool.fs.edit.failed{ "stale read — re-read the file" }
else                     → apply → tool.fs.edit.result{ path, sha }
```

- **Structural half.** `fs.seen` evaluated at the call's position is a fold
  over the call's causal past — which equals the context the emitting
  firing consumed (doc 19: position coincidence). A key for the path means
  the model has genuinely seen this file. `on:` includes `edit.result` and
  `write.result` so a read → edit → edit chain does not force a pointless
  re-read — the model *did* see the latest version (it wrote it; the result
  event attests the sha).
- **Freshness half.** The context's sha must equal the disk's at execution
  time. A parallel branch or another session changing the file ⇒ mismatch ⇒
  failed ⇒ the model must re-read. "The context did not contain the latest
  version", caught exactly.
- There is no `base_sha` in the call payload: the log already carries the
  evidence, and making the model thread the sha by hand would duplicate the
  projection with a new failure mode (a hallucinated sha). The audit fold
  ("was every edit preceded by a read", 12 W4) is the same projection
  evaluated offline.
- Cross-request editing ("now also fix the comment") works because a
  session is a causal chain (doc 19): the previous request's read is in the
  new edit's causal past.

### Deterministic single-purpose tools (the bash replacement)

Instead of a general bash tool — which would mean parsing arbitrary shell and
gating it — each external action is a named tool that does one deterministic
thing:

| Kind | Call payload | Result payload |
|---|---|---|
| `tool.fmt.run` | `{ path? }` | `{ changed: [path] }` |
| `tool.lint.run` | `{ path? }` | `{ findings: [{ path, line, rule, msg }], ok }` |

`fmt` is the project formatter (gofmt/prettier per workspace), `lint` is the
read-only linter. Both are pure, idempotent, and need no permission gate
because they cannot do anything but their one job. New deterministic actions
(`test.run`, `typecheck.run`) are added the same way later; the minimal target
is `fmt` + `lint`.

A later option, not minimal-target machinery: edit → fmt → lint as a
**deterministic chain** — `fmt` subscribing to `tool.fs.edit.result`
directly (doc 18: chains are topology). In the minimal target the brain
calls all three explicitly; once the trace corpus shows it always does, the
chain is a crystallization rewrite (doc 08), not a redesign.

## Scope: confinement is plugin rooting, not a bus gate

The agent must not escape the working directory. In the minimal target this is
**not** a bus-level permission check (no gates, per the use case) — it is a
property of the plugin: each fs/fmt/lint binary is launched rooted at the
workspace,

```
plugins/fs --socket /tmp/reflex.sock --root /abs/path/to/workspace
```

and resolves every `path` against `root`, rejecting absolute paths and any
`..` escape:

```
resolved := filepath.Join(root, filepath.Clean("/"+path))   // clamp to root
if !under(root, resolved) { return failed{ "path escapes scope" } }
```

A path outside scope becomes a `tool.fs.*.failed` event — an error the
topology sees, not a crash. The bus-level scope/permission model
([06-permissions-and-scopes.md](./06-permissions-and-scopes.md)) is the future
hardening (who may write which path, recursive grant); the minimal coding agent
does not need it, and rooting is enough to make "cannot leave CWD" true.
This placement is also what the engine's domain-blindness demands (doc 16 /
19): the guard reads payloads, so it lives in domain code, never in
dispatch.

## The loop

One declared scope, one node — the payoff of doc 15's reduction (the six
nodes of the original `react.yaml` collapse into subscriptions and engine
machinery):

```yaml
scopes:
  - name: request
    root: request.received
    budget: { llm.completed: 12 }   # the loop cap, at the right site (doc 16)

projections:
  - name: transcript                # the brain's context, a declared view (doc 19)
    on:    [request.received, tool.*.result, tool.*.failed,
            assistant.message, scope.budget_low]
    in:    session
    shape: log
  # fs.seen is registered by the fs plugin, not declared here

nodes:
  - name: brain
    type: llm
    on:    [request.received, scope.brain.closed]
    emit:  [tool.*.call, assistant.message, request.handled]
    reads: [transcript]
    config: { model: gemini-3.5-flash }
    # tools are NOT listed — the menu is the projection of tool.*.call consumers
```

What is *not* here, and why:

- **No seed, no pumps, no `llm.turn`** — the brain subscribes directly to
  `request.received` and to its own node-rooted scope's closure (doc 15);
  each firing that emits calls roots a scope instance, and
  `scope.brain.closed` is the next observation.
- **No error pump and no decode** — `tool.*.failed` is non-terminal inside
  the brain scope, so the cone still quiesces and the failure sits in the
  closed cone's fold for the brain to see and decide (doc 16). Output
  parsing is `llm` config (the action allowlist), not a node. An error
  lane (args-repair / medic, doc 18) is an optional later addition.
- **No `loop: max_iterations`** — the cap is the request scope's budget;
  `scope.budget_low` lands in the transcript one firing ahead of the
  guillotine, so the model can wrap up with what it has (doc 16).

## Deltas from current code

1. **Structured tool payloads.** `tool_node`'s `{args}`→`{result}` string
   shape becomes per-tool JSON schemas (table above). The biggest change.
2. **Out-of-process tool plugins.** Builtin `calc/echo/length/upper` live in
   the bus; `fs/fmt/lint` are SDK Remote plugins under `plugins/`. The
   transport exists; the tools and the workspace dir are new.
3. **Plugin rooting** for scope confinement (`--root`, path clamp). New, small,
   self-contained in each plugin.
4. **Model-doc preconditions**: node-rooted scopes with per-scope online
   quiescence (docs 16/17 — `scope.brain.closed` is what the loop rests on),
   the projection machinery (`reads:`, attach-at-dispatch, plugin-registered
   projections — doc 19), and the subject/trace envelope with wildcard
   matching (doc 13).
5. **The plugin obligation rule** (doc 17): the obligation opened by
   dispatching `tool.fs.*.call` closes when the **result event is
   appended**, not when the call is delivered — a pending plugin call holds
   the brain scope open. Without this the barrier fires early.
6. **`plugins/` workspace root** — out of the runtime core, gitignored build
   artifacts, one binary per tool family.

## Open questions

- **Edit vs whole-file.** `edit{old,new}` is proposed as primary; if small
  models struggle to produce a unique `old`, fall back to `write` with full
  content. Decide after one live run, not before.
- **One multi-tool plugin binary vs one per family.** One binary registering
  all `fs.*` kinds is fewer processes; per-tool is cleaner isolation. Cardinal
  for the `plugins/` layout but reversible.
- **Search backend.** Literal/regex walk in-process vs shelling to `rg`.
  Shelling is faster but reintroduces a process the rooting must contain;
  in-process keeps the determinism promise. Lean in-process for the minimal
  target.
