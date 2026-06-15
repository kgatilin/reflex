# topologies — operator documents for `reflexd apply`

Declarative topology documents (the `pkg/topology` schema): scopes, subscribers,
projections, and the domain event catalog. A document names a body by **kind +
config** (a serialisable descriptor), never a Go closure, so it applies through
the daemon and recovers from the log (G8). Body kinds: `llm` (the reasoning
core), `entry` / `gate` / `verifier` (the control bodies), and the launched
plugins (`fs`, `pytest`) which self-register — the document never writes a
`plugin` node, it just emits the kinds the hands consume.

## coding-agent.yaml

The doc-29 §4b state-defined coding agent: a brain that loops read→edit→test over
a checkout, **completion as a verified state** (the `verifier` re-runs the agreed
check and writes `done` only if it passes), bounded by the `request` scope budget.

It declares only domain kinds and wiring; the **fs + pytest hands self-register
when the daemon launches them**, so it must be applied to a daemon started with
`serve --root` (which brings those hands up). Applied to a daemon without them,
`apply` correctly reports the tool kinds as gaps — the graph is only connected
once the hands are present. (`reflexd validate coding-agent.yaml` is therefore a
partial dry-run: it checks the document in isolation and will list the tool kinds
as unconsumed; that is expected.)

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
