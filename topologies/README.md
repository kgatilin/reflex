# topologies — operator documents for `reflexd apply`

Declarative topology documents (the `pkg/topology` schema): scopes, subscribers,
projections, and the domain event catalog. A document names a body by **kind +
config** (a serialisable descriptor), never a Go closure, so it applies through
the daemon and recovers from the log (G8). Body kinds: `llm` (the reasoning
core), `entry` / `gate` / `verifier` (the control bodies), and `plugin` (an
out-of-process hand like `fs` / `pytest`). A launched plugin exposes a
**capability** (the kinds it can handle + their schemas); USING it is a
subscription the document declares — a `plugin`-bodied subscriber referencing the
plugin by name (`config: {plugin: <name>}`), with an operator-chosen scope.

## coding-agent.yaml

The doc-29 §4b state-defined coding agent: a brain that loops read→edit→test over
a checkout, **completion as a verified state** (the `verifier` re-runs the agreed
check and writes `done` only if it passes), bounded by the `request` scope budget.

It declares the agent wiring **including** the `fs` + `pytest` handler
subscriptions (scoped `In: request`, referencing the plugins by name). Those
references resolve only against a daemon that has **launched** the plugins, so it
must be applied to a daemon started with `serve --root` (which launches them); a
reference to an unlaunched plugin is rejected. (`reflexd validate
coding-agent.yaml` standalone — no daemon — cannot resolve the plugin references
and is therefore only a partial dry-run.)

### Run it (Iteration 4 — local, real model)

The model is Gemini on Vertex (`vertex:gemini-2.5-pro`); edit `body.config`'s
`project` / `location` for your GCP project, and ensure Application Default
Credentials are available (`gcloud auth application-default login`). The
`verifier`'s `command` (`python -m pytest -q`) runs in the daemon's working
directory — start the daemon from the checkout, or set the verifier's `dir`.

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
to a terminal verdict: `done` (the verifier saw green) or `failed` (the loop hit
the budget backstop). The status rides on the `request.terminal` payload.

### Boundaries (doc 29 §2)

Nothing here is SWE-bench-specific. The benchmark glue — checking out a
`base_commit`, running inside the instance's Docker image, scoring with the
official harness — is throwaway bash **outside** the framework (Iteration 5). This
document is the generic agent; the verifier runs the repo's own tests, which is
self-verification, not the harness's final word.
