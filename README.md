# reflex

> An event-sourced agent kernel on a converged substrate: three primitives —
> **Event**, **Reaction**, **Projection** — and nothing else. No agent loop, no
> orchestrator. Behaviour is the transitive closure of reactions over an
> append-only log; the log is the only source of truth.

Canonical design lives in [`docs/CONCEPT.md`](docs/CONCEPT.md). The current
north star is [`docs/29-swebench-target.md`](docs/29-swebench-target.md) (drive
one real SWE-bench Lite instance end-to-end on the new stack); engine-internal
stages are tracked in [`docs/28-substrate-rebuild-roadmap.md`](docs/28-substrate-rebuild-roadmap.md).

## The model

Everything that happens is an **Event** on an append-only log. Behaviours are
**Reactions**: a Reaction matches events and returns `[]Emit` (parallel
fan-out). A **Projection** is a pure fold of the log — state is never stored, it
is recomputed. The LLM is a Reaction. A file tool is a Reaction. The whole agent
is `event → reaction → new events → …`, terminating when the log quiesces.

Two invariants carry the design:

- **Uprightness (G8):** anything not recomputable from the log is a bug. The
  live topology itself is a *fold* of `sys.topology.changeset.*` facts on the
  log, not a config slice — applying, replaying, and reloading all converge.
- **Completion is a verified state**, never an LLM claim and never mere
  quiescence. "Done" is a value a verifier writes.

## Layout

```
engine/          the kernel: Event/Reaction/Projection, scopes + budgets + closure,
                 catalog + view types, the connectivity validator, dispatch/drain,
                 and the control plane (topology as a fold of changeset facts)
nodes/           built-in reaction bodies over the kernel — body-agnostic engine,
                 bodies resolved by name from a process registry
  nodes/llm      the llm body (real model via pkg/provider → allowlisted emits + llm.usage)
  nodes/tool     a built-in tool-shaped reaction body
pkg/topology/    the declarative topology document (YAML/JSON) → engine.Decls
pkg/daemon/      long-lived engine host: composition root + mutex-guarded
                 Apply/Validate/Emit/Topology/Events + Load (replay) + unix-socket HTTP API
pkg/provider/    neutral completion interface + real Vertex adapters (Gemini, Claude, OSS)
cmd/reflexd/     the daemon + control-plane CLI
docs/            CONCEPT.md (canonical) + numbered design notes; docs/outdated/ is the archive
```

## reflexd

`reflexd` hosts one engine behind a unix socket; the other verbs are clients of
that API.

```
reflexd serve [--socket PATH]          # host the engine
reflexd apply FILE                     # apply a topology document (live config)
reflexd emit --subject S --payload J [--wait KIND] [--no-drain]
reflexd topology                       # print the live table
reflexd events                         # print the log
reflexd validate FILE                  # local dry-run: connectivity gap report (no daemon)
```

`emit --wait <kind>` is the send-message / drive-to-terminal verb: it appends an
ingress event, drains the engine, and asserts the named kind occurred. `apply`
against a running daemon is live topology config — adding a node does not require
a restart, because the topology is a fold of the log.

A node's behaviour is **code resolved by name**, not a fact: the
`sys.node.registered` fact carries a serializable `body_kind` + `body_config`
descriptor, and an injected resolver (the `nodes` factory registry) rebuilds the
runnable `Reaction`. `engine.Load(log, …)` reconstructs the whole engine —
topology and bodies — from the log alone.

## Build & test

```
make build      # go build ./...
make test       # go test ./...
make vet        # go vet ./...
make install    # go install ./cmd/reflexd
```

Discipline: build/vet clean, test with `-race`, commit every self-contained unit.
