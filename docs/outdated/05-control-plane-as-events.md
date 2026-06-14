# 05 — Control plane as events

Phase 4b status: shipped. Subscriptions are managed via events on the
bus. The YAML config is a seeded stream of control-plane events. The
handler graph IS the live subscription table; the parsed-YAML model is
retained only as a fast pre-flight check. Audit logging is an ordinary
handler subscribed to control-plane events. Phase 4c (shipped) gates
every handler-issued control-plane mutation through a synchronous
permission check (`SubscribeAs`/`UnsubscribeAs`/`HandlerDeregisterAs`
on `*bus.Bus`); the four boot-time paths (`Register` and friends) keep
the bus-authoritative semantics they had in 4b.

## The shift

In Phases 1–3, the topology is two-layered: a YAML config sits
alongside the bus, the static graph compiles it at startup, and the bus
holds a private `subscribers []Subscriber` list (`pkg/bus/bus.go`).
Wiring is a one-shot at boot.

Phase 4b promotes wiring to first-class events. The bus has a single
`live_table` projection — a fold of the control-plane event stream —
which holds the current subscription set. Every operation that mutates
the topology emits an event. The bus's "subscribers list" is no longer
private state; it is the projection.

YAML doesn't go away; it becomes a seeded stream. `reflex run config.yaml`
is mechanically:

```
1. Parse config.yaml into a list of HandlerRegistered + Subscribed events.
2. Publish them to the bus.
3. Publish the domain seed event.
4. Drain.
```

A new YAML file with a different topology produces a different seed
stream. A running bus that wants to swap topologies emits
`Unsubscribed` + `Subscribed` events. Both paths go through the same
control-plane.

## Control-plane events

Implemented in `pkg/bus`. All five are terminal.

```
HandlerRegistered{name, consumes, emits, description}
```

Announces a handler to the bus. Carries the descriptor shape. Until a
`HandlerRegistered` has been seen for `name`, no
`Subscribed{handler_name: name, ...}` can take effect (`SubscribeWithCheck`
returns an error and emits `SubscriptionRejected`).

```
Subscribed{handler_name, event_type, max_iterations?}
```

Binds a handler to an event type. `max_iterations` carries the loop cap
when the binding is part of a declared loop. Multiple bindings per
handler (notably the audit handler reacting to N control-plane types)
produce multiple `Subscribed` events.

```
Unsubscribed{handler_name, event_type}
```

Removes a binding. Idempotent — removing a non-existent binding is a
no-op.

```
HandlerDeregistered{handler_name}
```

Removes the handler and all its bindings. The bus emits one
`Unsubscribed` per remaining binding followed by one
`HandlerDeregistered` so the trace records the full picture.

```
SubscriptionRejected{handler_name, event_type, reason}
```

Emitted when `SubscribeWithCheck` refuses a binding. Today the only
rejection reasons are "handler is not registered" and "would introduce
uncapped cycle: H1 -> H2 -> H1". The subscription does NOT take effect
when this event fires.

Control-plane events emitted outside an active `Run` (boot-time YAML
seeding, or test setup) are queued and delivered on the next `Run` so
audit handlers can react to them even though no domain event has yet
fired. They are excluded from the `EventDispatched` meta-event class so
N registrations do not produce N×EventDispatched noise.

## The live table

```go
type LiveTable struct {
    Handlers       map[string]HandlerSpec       // name → spec
    Subscriptions  map[string][]Subscription    // event_type → subscriptions
    Scopes         map[string]Scope             // name → scope (Phase 4c)
}

type Subscription struct {
    Handler  string
    Filter   string  // empty in 4b, non-empty in 4c+
}
```

The live table is a projection. It is rebuildable from the control-plane
event stream by replaying `HandlerRegistered` / `Subscribed` /
`Unsubscribed` / `HandlerDeregistered` in order. It is held in memory
for dispatch performance; losing it cannot disagree with the log because
the log is the source.

The dispatcher consults the live table when fanning out an event: for
type `T`, the matching subscribers are exactly `Subscriptions[T]` (modulo
filters in 4c). The static graph builder of Phase 1.5 becomes a
read-side query over the live table.

## Cycle detection over the live table

Phase 1.5's cycle detector (`pkg/graph/graph.go`) runs Tarjan's
algorithm over the YAML-derived graph. Phase 4b ports it to the live
table at the bus layer (`pkg/bus/bus.go` —
`liveTableHasUncappedCycle`).

The static graph builder is retained as a defence-in-depth pre-flight:
parsing the YAML and running Tarjan over the parsed nodes is cheap,
catches typos early, and matches what `reflex validate` exposes to
config authors. The authoritative check is the live-table one — the
runtime `Build` calls `bus.CheckLiveTableCycles()` after all YAML
handlers have been registered, and any runtime `SubscribeWithCheck` is
gated by the same algorithm.

A runtime subscription that would close an uncapped cycle is refused
synchronously: the bus emits
`SubscriptionRejected{handler_name, event_type, reason}`, the binding
is NOT added, and the caller of `SubscribeWithCheck` receives a
non-nil error. Phase 4c will additionally have the permission enforcer
treat the rejection as a policy event and report
`PermissionDenied{principal, op: subscribe, ...}` against the
originating handler.

This composes cleanly with the seeded-from-YAML flow: the cycle check
runs whether the binding came from YAML at boot or from a Phase 6
optimisation pass.

## Compression operations as subscription operations

A merge of two handlers `A` and `B` into a composite `AB`:

```
1. HandlerRegistered{name: AB, type: composite, consumes: ..., emits: [...]}
2. Subscribed{handler: AB, event_type: T1}        // every type A or B subscribed to
   Subscribed{handler: AB, event_type: T2}
   ...
3. Unsubscribed{handler: A, event_type: T1}
   Unsubscribed{handler: A, event_type: T2}
   ...
4. Unsubscribed{handler: B, ...}
5. HandlerDeregistered{name: A}
6. HandlerDeregistered{name: B}
```

Six events; no special "merge" primitive in the bus. The optimiser
(Phase 6, see [`08-optimization-as-rewrite.md`](./08-optimization-as-rewrite.md))
emits this sequence as a single transaction. The cycle detector reacts
to the `Subscribed` events; the audit logger reacts to all six; the
permission enforcer checks each event against the optimiser's scope
grants.

Other rewrites:

- **Dead-edge prune**: a single `Unsubscribed{handler: X, event_type:
  T}` where the trace corpus shows `Subscriptions[T]` never fired for
  `X`.
- **Pass-through collapse** (A → B → C, B has no emit other than
  forwarding): `Unsubscribed{B}` + `Subscribed{C → A's emit type}`.
- **Filter narrowing**: `Unsubscribed` followed by `Subscribed` with a
  more specific `filter`.

None of these require new primitives on the bus.

## Audit as a handler

The audit logger ships as the `audit` handler type (`pkg/handler/audit.go`):

```yaml
- name: audit
  type: audit
  on: HandlerRegistered    # cosmetic — the handler matches its full set
  config:
    sink: file:///var/log/reflex-audit.jsonl
```

It is an ordinary handler with no privileged access. It subscribes to
all five control-plane event types (`HandlerRegistered`, `Subscribed`,
`Unsubscribed`, `HandlerDeregistered`, `SubscriptionRejected`) and
writes them to the configured sink as JSONL. Supported sinks today:
`stderr` (default), `stdout`, `file:///path`. Phase 4c will add the
permission events to the audited set without changing the handler API.

The audit handler uses the bus's `MultiConsumes` descriptor field — one
`HandlerRegistered` event for the handler, one `Subscribed` per audited
type — so the boot-time control-plane log fully describes the audit
handler's reach. See `examples/control_plane_audit.yaml` for a runnable
end-to-end demonstration.

Auditors are not built into the framework; they are configured. A
deployment that doesn't want audit doesn't declare one. A deployment
that wants multiple audit destinations (file + S3 + syslog) declares
three.

## Permission enforcement at the bus edge

Phase 4c gates every handler-issued control-plane mutation through a
synchronous check at the bus edge. Handlers don't go through
`Register`/`SubscribeWithCheck` directly — they use the
principal-attributed APIs `SubscribeAs(principal, …)`,
`UnsubscribeAs(principal, …)`, `HandlerDeregisterAs(principal, …)`,
`PublishPermissionGranted(principal, …)`,
`PublishPermissionRevoked(principal, …)`.

On a refused operation the bus emits
`PermissionDenied{principal, op, target_scope, reason}` and leaves the
live table untouched. The audit handler (now subscribed to the three
new permission types as well) records the denial on the same log as
every successful mutation.

Boot-time YAML loading uses the older bus-authoritative paths
(`Register` + `SubscribeWithCheck`) and is exempt from the permission
check — the loader IS the root authority. See
[`06-permissions-and-scopes.md`](./06-permissions-and-scopes.md) for
the full grammar, matching semantics, default-protected zones, and
recursive `meta.grant` rule.

## Boot sequence

```
1. Bus starts with an empty live table.
2. Seed: HandlerRegistered{name: enforcer, scope: {meta.grant: [*]}}
        Subscribed{enforcer → HandlerRegistered}
        Subscribed{enforcer → Subscribed}
        Subscribed{enforcer → Unsubscribed}
        Subscribed{enforcer → PermissionGranted}
        ... (bootstrap permission rules)
3. Seed: HandlerRegistered{name: audit, scope: {read: [*]}}
        Subscribed{audit → *}
4. Seed: HandlerRegistered + Subscribed events for the domain handlers
   from the YAML config.
5. Seed: HandlerRegistered + Subscribed for the analyzer / optimiser /
   archmotif subscribers (if configured).
6. Seed: the domain trigger event (RequestReceived / MessageReceived /
   …).
7. Drain.
```

Steps 2–5 are "control-plane seeding"; step 6 is the only domain event.
The audit logger sees every step from step 3 onwards (its subscription
starts there). The permission enforcer sees every step from step 2
onwards (its subscription starts at step 2 — root, which has
`meta.grant: [*]`).

The framework provides exactly two primitives at the boot layer: append
an event, fan out to live-table subscribers. Everything else — audit,
permission, validation, analysis — is configured as handlers.

## Why this matters

The split between "config-time topology" and "runtime topology" is a
common source of bugs. Drift between the two requires reconciliation
logic and produces silent failures (a handler declared in YAML that
never actually got registered; a runtime subscription that doesn't
match the config).

Promoting control-plane operations to events removes the split. There
is one table, one source of truth, one event stream feeding it. The
audit log is the diff history. The cycle detector and the permission
enforcer are subscribers like any other.

Phase 4b is the structural shift that unlocks Phase 6 (compression as
graph rewrite) and Phase 7 (archmotif as live subscriber). Both depend
on subscription change being observable on the bus, not deduced from
parsed config.
