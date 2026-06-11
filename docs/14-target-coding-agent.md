# 14 — Target: a minimal coding agent from plugins

The first concrete target the runtime must carry end to end: a coding agent
that makes small, real code changes. It is the ReAct topology of
[12-react-experiment.md](./12-react-experiment.md) with file tools instead of
`calc`, the subject/trace envelope of [13-event-taxonomy.md](./13-event-taxonomy.md),
and every tool living as an **out-of-process plugin** registered over the
existing SDK Remote transport. This document defines the use case, the tool
signatures, the scope confinement, and the deltas from current code. It is a
target, not a phase — it names what "done" looks like.

## The use case

One request, one small code change, fully auditable:

> "In `pkg/foo/bar.go`, the function `Parse` returns `nil` on an empty input;
> make it return an error instead."

The agent must be able to: **find** where the symbol lives, **read** the file,
**edit** it surgically, **format** and **lint** the result, and answer — never
touching anything outside the working directory. That is the whole minimal
scope. No bash, no permission prompts, no multi-file refactors.

A live trace (ReAct loop, one tool per turn):

```
app.session.S.request.received{ text: "...make Parse return an error..." }
  brain   -> tool.fs.search.call{ query: "func Parse" }
  search  -> tool.fs.search.result{ matches: [{ path: "pkg/foo/bar.go", line: 12 }] }
  brain   -> tool.fs.read.call{ path: "pkg/foo/bar.go" }
  read    -> tool.fs.read.result{ content: "...", sha: "ab12", lines: { from: 1, to: 40, total: 40 } }
  brain   -> tool.fs.edit.call{ path: "pkg/foo/bar.go", old: "return nil", new: "return errEmpty", base_sha: "ab12" }
  edit    -> tool.fs.edit.result{ sha: "cd34", replaced: 1 }
  brain   -> tool.fmt.run.call{ path: "pkg/foo/bar.go" }
  fmt     -> tool.fmt.run.result{ changed: ["pkg/foo/bar.go"] }
  brain   -> tool.lint.run.call{ path: "pkg/foo/bar.go" }
  lint    -> tool.lint.run.result{ ok: true, findings: [] }
  brain   -> assistant.message(T){ text: "Parse now returns errEmpty on empty input." }
           + request.handled(T)
```

Whether the change was verified (lint `ok` before the final message) is a
projection over the cone — the auditability win of 12-react-experiment.md W4,
for free.

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
model gains a tool.

## Tool signatures

Tools carry **structured** payloads — the current `{args: string}` shape
(`pkg/handler/tool_node.go`) is insufficient for files and is the first delta.
Each tool does exactly one thing; none parse shell.

### File tools (`fs`)

| Kind | Call payload | Result payload | Fails when |
|---|---|---|---|
| `tool.fs.read` | `{ path, offset?, limit? }` | `{ path, content, sha, lines: { from, to, total }, truncated }` | missing, outside scope |
| `tool.fs.edit` | `{ path, old, new, replace_all?, base_sha? }` | `{ path, sha, replaced }` | `old` absent / non-unique, sha mismatch, no prior read |
| `tool.fs.write` | `{ path, content }` | `{ path, sha }` | outside scope |
| `tool.fs.search` | `{ query, glob?, regex?, max? }` | `{ matches: [{ path, line, text }], truncated }` | — |

The three file tools are a deliberate division of labour — **read a window,
edit in place, write only to create**:

- `read` returns a **window**, not the whole file: `offset`/`limit` (by line,
  with sane defaults) so the model pulls only the slice it needs. Content is
  **line-numbered**, and `lines.total` + `truncated` tell the model there is
  more — never a silent cut. The line numbers are what `edit` targets against.
- `edit` is the **update primitive** and the primary code-change tool: an
  **exact** `old`→`new` replacement, in place, never a rewrite. `old` must be
  **unique** or the call fails (no guessing); `replace_all` is the explicit
  opt-in for a deliberate multi-site change. `replaced` reports the count.
- `write` is create / full overwrite **only** — for new files. It is *not* the
  way to change an existing file; to update, `edit`. (A `write` over an
  existing path is allowed but is the wholesale-rewrite escape hatch, not the
  default path.)
- **Read-before-edit** is enforced as a projection, not a lock: `edit` requires
  a prior `tool.fs.read.result` for the same `path` in the cone, and `base_sha`
  must match it — so the model edits content it has actually seen, at the
  version it saw. No read of this path before the edit ⇒ fail. The guard is a
  fold over the cone (who read this path, at what sha), in the same spirit as
  the verification check of 12-react-experiment.md — no stored flag.
- `sha` is the content hash; `base_sha` is the optimistic-concurrency
  precondition — load-bearing once sessions run in parallel and a cached/late
  write could clobber (13-event-taxonomy.md concurrency notes).
- `search` is the minimal "find where X is": literal by default, opt-in
  `regex`, optional `glob` filter, `max`-capped with a `truncated` flag (no
  silent truncation).

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

## The loop

The topology is `react.yaml` with the tool set swapped — brain reasons one step
at a time, decode translates the completion into one `tool.fs.*.call`, the tool
plugin answers, an echo pump feeds the result back as the next `llm.turn`, and
the loop cap lives on `brain` (12-react-experiment.md). Sketch:

```yaml
handlers:
  - name: seed
    type: echo
    on: request.received
    config: { emit: llm.turn }

  - name: brain
    type: llm_gemini
    on: llm.turn
    emits: [llm.completed]
    config:
      model: gemini-3.5-flash
      # tools are NOT listed here — the menu is the projection of
      # tool.*.call consumers (the registered plugins)
    loop: { max_iterations: 12 }

  - name: decode
    type: llm_decode
    on: llm.completed
    emits: [tool.fs.read.call, tool.fs.edit.call, tool.fs.search.call,
            tool.fmt.run.call, tool.lint.run.call,
            assistant.message, request.handled]

  # tool.* nodes are the out-of-process plugins, registered over the socket —
  # not declared here. The pumps close the loop generically:
  - name: pump
    type: echo
    on: tool.*.result        # one generic pump, all tools (13: tool.*.result)
    config: { emit: llm.turn }
  - name: pump-fail
    type: echo
    on: tool.*.failed
    config: { emit: llm.turn }
```

The two generic pumps (`tool.*.result`, `tool.*.failed`) replace the per-tool
echoes of the original `react.yaml` — the payoff of wildcard subjects.

## Deltas from current code

1. **Structured tool payloads.** `tool_node`'s `{args}`→`{result}` string
   shape becomes per-tool JSON schemas (table above). The biggest change.
2. **Out-of-process tool plugins.** Builtin `calc/echo/length/upper` live in
   the bus; `fs/fmt/lint` are SDK Remote plugins under `plugins/`. The
   transport exists; the tools and the workspace dir are new.
3. **Plugin rooting** for scope confinement (`--root`, path clamp). New, small,
   self-contained in each plugin.
4. **Subject/trace envelope** (13-event-taxonomy.md) and **wildcard matching**
   (`tool.*.result` pumps) are preconditions.
5. **`plugins/` workspace root** — out of the runtime core, gitignored build
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
