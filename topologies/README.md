# topologies — operator documents for `reflexd apply`

Declarative topology documents (the `pkg/topology` schema): scopes, plugins,
subscribers, projections, and the domain event catalog. A document names a body
by **kind + config** (a serialisable descriptor), never a Go closure, so it
applies through the daemon and recovers from the log (G8). Body kinds: `llm` (the
reasoning core), `entry` / `gate` / `verifier` (the control bodies).

An out-of-process hand (`fs`, `pytest`) is **not** a body kind — it is
scope-agnostic infrastructure registered in the `plugins:` section (HOW to launch:
command + transport). The process announces, in its hello, which kinds it handles
and emits. The **handler is a normal subscriber** — its own `on` / `in` / `emits`,
no plugin reference — and the daemon backs it with the plugin that *handles* its
subscribed kind (the link is the kind). So a hand is just a vanilla subscriber
named after the tool-call kind it serves, scoped like any other.

## coding-agent.yaml

The doc-29 §4b state-defined coding agent: a brain that loops read→edit→test over
a checkout, bounded by the `request` scope budget. **Completion is a verified
state, never the model's word.** The brain never calls a "done" function — a turn
that calls *no* function is its claim of completion (prose → `llm.message`,
silence → `llm.empty`). That claim is then evaluated independently: the `verifier`
re-runs the agreed test command (red → `verify.failed` back into the loop), and on
green an independent `judge` (just another `llm` node) reviews the change for
gaming the tests. Only a judge `judge.approved` writes `state.updated.status{done}`.

The `fs` + `pytest` hands are launched by `serve --root` (confined to the
checkout); the document's vanilla handler subscribers (`tool.fs.read.call`, …) are
backed by them at apply time. So this doc must be applied to a daemon started with
`serve --root`; a handler whose kind no launched plugin serves stays an ordinary
consumer. (`reflexd validate coding-agent.yaml` standalone — no daemon — cannot
back the handlers and is therefore only a partial dry-run.)

### Run it (Iteration 4 — local, real model)

The model is Gemini on Vertex (`vertex:gemini-3.5-flash`); edit `body.config`'s
`project` / `location` for your GCP project, and ensure Application Default
Credentials are available (`gcloud auth application-default login`). The
`verifier`'s `command` (`python -m pytest -q`) runs in the daemon's working
directory — start the daemon from the checkout, or set the verifier's `dir`.

The daemon's environment must have a `python` with `pytest` installed (the
`pytest` plugin and the `verifier` both shell out to it). One gotcha the live
drive hit: put the interpreter's `bin` on `PATH` as an **absolute** path — Go
refuses to exec a program resolved through a relative `PATH` entry
(`exec: "pytest": cannot run executable found relative to current directory`).
Keep `body.config.max_tokens` generous (the brain and judge both reason before
the visible turn).

```sh
# 1. build + install the daemon
make -C .. install                       # or: go install ./cmd/reflexd

# 2. host the engine from the target checkout, with the hands confined to it
cd /path/to/checkout
reflexd serve --root "$PWD" &            # launches fs + pytest plugins

# 3. apply the agent topology (folds the launched hands; validates the whole graph)
reflexd apply /path/to/reflex/topologies/coding-agent.yaml

# 4. drive one task to terminal — feed the issue text, wait for the terminal fact
reflexd emit --subject task.new \
  --payload '{"task":"<the issue text>"}' \
  --wait request.terminal

# 5. inspect: the produced diff is in the checkout's working tree
git -C /path/to/checkout diff
```

`emit --wait request.terminal` blocks until the agent drives its `request` scope
to a terminal verdict: `done` (the verifier saw green **and** the judge approved)
or `failed` (the loop hit the budget backstop). The status rides on the
`request.terminal` payload.

### Boundaries (doc 29 §2)

Nothing here is SWE-bench-specific. The benchmark glue — checking out a
`base_commit`, running inside the instance's Docker image, scoring with the
official harness — is throwaway bash **outside** the framework (Iteration 5). This
document is the generic agent; the verifier runs the repo's own tests, which is
self-verification, not the harness's final word.
