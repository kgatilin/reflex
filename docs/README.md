# reflex design documentation

This directory captures the full mental model and per-phase design of
reflex — an event-sourced agent runtime in which every component (handlers,
control plane, audit, analysis, permissions, human feedback) lives as a
subscriber on a single bus, with no synchronous primitives.

Start with [`01-mental-model.md`](./01-mental-model.md). For a one-page
cheat sheet of the settled model (envelope, subjects, event catalog, node
types) see [`00-reference.md`](./00-reference.md). If you just want to drive
reflex from another application, jump to
[`09-embedding-api.md`](./09-embedding-api.md). For phase status, see
[`10-phase-roadmap.md`](./10-phase-roadmap.md).

## Recommended reading order

1. [`01-mental-model.md`](./01-mental-model.md) —
   the synthesis: events-only, graph ≡ subscription table, bus self-hosts,
   projection-as-truth, no privileged plane.
2. [`02-handlers-and-schemas.md`](./02-handlers-and-schemas.md) —
   the YAML grammar, self-describing handlers, event schemas, the
   terminal-event invariant.
3. [`03-bus-and-projection.md`](./03-bus-and-projection.md) —
   the bus contract, meta-events, the projection store, the aggregator
   pattern, CLI wait-predicates.
4. [`04-static-and-runtime-analysis.md`](./04-static-and-runtime-analysis.md) —
   load-time cycle detection (Tarjan), runtime trace analyzer, archmotif
   adapter, objective function, the planned migration to live-table
   analysis.
5. [`05-control-plane-as-events.md`](./05-control-plane-as-events.md) —
   subscriptions themselves as event streams; YAML config as a seeded
   stream; compression / audit / enforcement as ordinary handlers.
6. [`06-permissions-and-scopes.md`](./06-permissions-and-scopes.md) —
   declarative scope + permissions, permission events, rogue-handler
   containment, recursive grant.
7. [`07-archmotif-as-live-subscriber.md`](./07-archmotif-as-live-subscriber.md) —
   archmotif as a bus-resident handler that maintains the runtime graph
   projection and drives the compression cycle.
8. [`08-optimization-as-rewrite.md`](./08-optimization-as-rewrite.md) —
   compression passes as graph rewrites that emit subscription events;
   feedback-as-rule; cost function; scope gating.
9. [`09-embedding-api.md`](./09-embedding-api.md) —
   the externally-facing API for foreign applications: Go `pkg/embed`,
   HTTP daemon, optional gRPC.
10. [`10-phase-roadmap.md`](./10-phase-roadmap.md) —
    every phase with status, scope, and dependencies.
11. [`11-domain-model.md`](./11-domain-model.md) —
    the distilled model: events, reactions, projections; errors-as-events,
    state-as-convention, scopes as structured concurrency over a causal DAG.
12. [`12-react-experiment.md`](./12-react-experiment.md) —
    findings from a live ReAct agent built from atomic nodes: what the
    three-concept model buys and which conventions are still missing.
13. [`13-event-taxonomy.md`](./13-event-taxonomy.md) —
    the wire shape: subjects (scope + kind), the trace envelope (correlation +
    causation), session resolution, and projections as wildcard subscriptions.
14. [`14-target-coding-agent.md`](./14-target-coding-agent.md) —
    the first end-to-end target: a minimal coding agent (read/edit/write/search
    + fmt/lint) built from out-of-process tool plugins, scoped to its workspace.
15. [`15-primitive-reduction.md`](./15-primitive-reduction.md) *(draft)* —
    collapsing the node vocabulary to two primitives (`llm` + `tool`); fan-out
    synchronization as the `scope.closed` projection, not a node.

## Convention

Each document opens with a 2–3 line summary, uses concrete YAML and
event-payload snippets where possible, and cross-links to neighbours. The
documents describe the system's design; they intentionally avoid
conversation-log shape ("we decided", "the user said") and read as a
specification a future reader can pick up cold.
