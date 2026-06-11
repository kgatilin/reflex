# 10 — Phase roadmap

Every phase with status, scope, and dependencies. Commit hashes cite the
point at which the phase landed on `main`. archmotif-side work is
tracked under its own repo and cited where reflex depends on it.

## Phases

| Phase | Status     | Scope                                                                                                                    | Depends on                  |
|-------|------------|--------------------------------------------------------------------------------------------------------------------------|-----------------------------|
| 1+2   | done       | terminal-event invariant + real-task triage pipeline (`parse_target`, `gh_query`, `triage_rules`)                       | —                           |
| 1.5   | done       | self-describing handlers + Tarjan static cycle detection over the YAML-derived graph + loop caps (`36e25bb`)            | 1+2                         |
| 3     | done       | analyzer engine + archmotif graph adapter + per-trace metrics + single-scalar objective (`df86e19`)                     | 1.5                         |
| 1.6   | done       | events-only model: bus meta-events, projection store, generic aggregator, CLI wait-predicates (`c545df4`)                | 1.5                         |
| 4a    | done       | bus daemon + remote handler SDK over a Unix socket (`c1b735c`)                                                            | 1.6                         |
| 4b    | done       | control-plane events + live-table cycle detector + daemon completeness (multi-handler mux, projection RPCs, await frames, CheckQuiescence)  | 4a                          |
| 4c    | done       | scope-based permission layer (`PermissionGranted`, `PermissionRevoked`, `PermissionDenied`) gated at the bus edge          | 4b                          |
| 4d    | pending    | scaffold CLI + tag archmotif release exposing `pkg/metrics` shim → drop the `go.mod replace` directive                   | 4a, archmotif side-quest    |
| 4e    | pending    | external embedder API (Go `pkg/embed` package + HTTP daemon + optional gRPC)                                             | 4a                          |
| 5     | pending    | embeddings on declared embeddable nodes + semantic-search API                                                            | 1.6                         |
| 6     | pending    | optimisation-as-rewrite live loop — compression passes via `LayerCollapseOp` emit subscription events within scope       | 4b, 4c                      |
| 7     | pending    | archmotif as bus-resident subscriber on the live subscription table                                                       | 4a, 4b, 4c                  |

## Cross-phase dependencies

```
1+2  ──► 1.5  ──► 3
            │
            └──► 1.6 ──► 4a ──► 4b ──► 4c ──► 6
                          │      │      │      │
                          ├──► 4d (archmotif tag)
                          ├──► 4e (embed API)
                          └─────────────────────► 7
                  └──► 5 (embeddings)
```

4a unblocks the daemon + SDK; 4b unblocks live-table operations; 4c
adds containment; 6 and 7 require 4b+4c. The embedder API (4e) and
the archmotif tag (4d) are parallel to 4b/4c — they only depend on the
Phase 4a daemon being in place.

## archmotif side-quest

Reflex's analyzer composes archmotif's matrix validators directly once
archmotif exposes a public `pkg/metrics` shim
(`Encoder` / `Operation` / `Interpreter` / `MatrixValidator` /
`LayerCollapseOp`). Side-quest landed under archmotif `e7e00ec` (a
different repo). Reflex Phase 4d consumes the tagged release and:

1. Drops the local `PowerDiagCycle` re-implementation in
   `pkg/analyzer/archmotif_adapter.go` in favour of
   `metrics.MatrixCycleValidator`.
2. Replaces the `go.mod replace` directive
   (`replace github.com/kgatilin/archmotif => /home/dev/dev/sandbox/archmotif`)
   with a proper module require on the tagged version.
3. Gains `LayerCollapseOp` for the Phase 6 cluster-collapse compression
   pass and the Phase 7 archmotif-resident pathology detector.

## What's already shipping

- The terminal-event invariant and the orphan watcher
  (`pkg/handler/unhandled_watcher.go` `CheckQuiescence`).
- The triage pipeline (`examples/triage.yaml`) operating against the
  real `gh` CLI.
- Self-describing handlers via `HandlerSpec` + the `Introspect`
  contract (`pkg/handler/handler.go`).
- Static cycle detection via Tarjan's algorithm
  (`pkg/graph/graph.go`).
- Loop caps enforced at runtime by the dispatcher
  (`pkg/bus/bus.go`'s `loopCaps`).
- The analyzer engine (`pkg/analyzer/`), including the archmotif
  graph adapter, per-request metrics, objective scoring, and a
  `--watch` directory driver.
- Bus meta-events (`EventDispatched`, `DrainQuiesced`,
  `HandlerFailed`), the projection store, the generic aggregator,
  and CLI wait-predicates (Phase 1.6, `c545df4`).
- `reflex emit / invoke / send` subcommands and YAML
  `events:[].cli` bindings (Phase 1.6).
- `reflex validate` and `reflex describe` (Phase 1.5).
- `cmd/analyzer` entry point with `--trace`, `--watch`, `--json`,
  `--metric`, `--request-id` flags.

## Phase 4a — bus daemon + remote handler SDK

Goal: split the bus into a long-lived daemon and a client SDK that
remote handlers (in separate processes, possibly different languages)
can use to subscribe. The wire transport is Unix domain socket; the
handler protocol is JSON event envelopes matching `pkg/event.Event`.

Out of scope for 4a:

- Live control-plane operations (4b).
- Permissions (4c).
- Multi-host daemons / network transport (deferred).

The daemon embeds the same bus as the in-process runtime. Remote
handlers register via `HandlerRegistered`, subscribe via `Subscribed`,
and emit events through the SDK's `Publish` method. The 4a
implementation can use a pre-Phase-4b boot-time registration model
(handler announces itself once, never deregisters) — that simplifies
4a without compromising 4b's design.

## Phase 4b — control plane as events + daemon completeness

Done. Five control-plane event types
(`HandlerRegistered`, `Subscribed`, `Unsubscribed`,
`HandlerDeregistered`, `SubscriptionRejected`) are first-class events
emitted by the bus on every subscription mutation. YAML loading is a
seeded stream of those events; the audit handler subscribes to them
and writes a JSONL log. The cycle detector is ported to the live
subscription table; a runtime `SubscribeWithCheck` that would close an
uncapped cycle is refused and a `SubscriptionRejected` event records
the rejection. The Phase 1.5 static check stays as a fast pre-flight.

The four Phase 4a daemon TODOs are also closed:

- `await` / `resolved` frames carry wait-predicates over the socket.
- `proj_get` / `proj_value` / `proj_set` frames give remote handlers
  full projection-store access.
- A single SDK Client connection now hosts N handlers; the daemon
  routes `deliver` frames by `handler_name`.
- The daemon runs `CheckQuiescence` after every successful
  `EmitAndDrain` so `RequestUnhandled` / `EventOrphaned` surface for
  socket-driven seeds.

See [`05-control-plane-as-events.md`](./05-control-plane-as-events.md)
and [`03-bus-and-projection.md`](./03-bus-and-projection.md).

## Phase 4c — scope-based permission layer

Done. Every handler declares a `scope:` (default `default.<name>`)
plus an optional `permissions:` block (`mutate` / `read` / `forbidden` /
`meta.grant`). The bus gates every handler-issued control-plane
mutation through a synchronous permission check at four
principal-attributed APIs (`SubscribeAs`, `UnsubscribeAs`,
`HandlerDeregisterAs`, `PublishPermissionGranted`/`Revoked`). On
refusal, the bus emits `PermissionDenied{principal, op, target_scope,
reason}` with reason `forbidden` / `out_of_scope` / `no meta-grant
authority`. Reserved zones `core.*` / `system.*` / `feedback.*` are
default-deny — even `mutate: [*]` does not authorise them; an explicit
pattern naming the reserved prefix is required. Recursive `meta.grant`
makes the grant tree rooted in the boot stream. See
[`06-permissions-and-scopes.md`](./06-permissions-and-scopes.md).

Per-`request_id` scoped permissions ("user X can only mutate handlers
tied to requests they started") are an explicit non-goal; Phase 4c is
handler-scoped only.

## Phase 4d — scaffold CLI + archmotif tag

Goal: ship a `reflex new <project>` scaffold that creates a skeleton
config + handler set; tag the archmotif release that exposes
`pkg/metrics` publicly; drop the `replace` directive in `go.mod`. The
analyzer then composes archmotif's matrix validators rather than
re-implementing them locally.

## Phase 4e — external embedder API

Goal: ship `pkg/embed` (Go in-process package), HTTP daemon endpoints,
optional gRPC mirror. The surface is small (emit / invoke /
projection-read / subscribe + describe), and the same semantics work
in-process and over the wire. See
[`09-embedding-api.md`](./09-embedding-api.md).

## Phase 5 — embeddings

Goal: declare embeddable nodes (handlers, events, projections) in
YAML; produce vector embeddings on each emission; expose a semantic
search API over the embedded corpus. Independent of the control-plane
work (uses 1.6 primitives only) and can land in parallel.

## Phase 6 — optimisation as rewrite

Goal: compression passes (subsumption, dead-edge prune,
pass-through collapse, cluster collapse via `LayerCollapseOp`) emit
subscription events within their permitted scope. The Phase 3
objective scalar is the cost function. Behavioural equivalence on the
trace corpus is the acceptance criterion. Human feedback enters as
ordinary rules under `feedback.*` scope guards. See
[`08-optimization-as-rewrite.md`](./08-optimization-as-rewrite.md).

## Phase 7 — archmotif as live subscriber

Goal: archmotif inside the bus, subscribed to control-plane and
meta-event streams, maintaining the runtime graph as a projection.
Drives the compression cycle by emitting `CompressionRequested` →
aggregator collects `CompressionContext` → archmotif emits
`CompressionProposed` → acceptance handler (auto or human-gated) →
patch events applied to the live table. archmotif has declared scope;
no privileged plane. See
[`07-archmotif-as-live-subscriber.md`](./07-archmotif-as-live-subscriber.md).
