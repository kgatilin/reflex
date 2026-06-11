# 05 — Control plane as events

Phase 4b vision. Subscriptions themselves are managed via events on the
bus. The YAML config becomes a seeded stream of control-plane events. The
handler graph IS the live subscription table; there is no separate parsed
model. Compression, audit, and permission enforcement all become ordinary
handlers subscribed to control-plane events.

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

```
HandlerRegistered{name, type, consumes, emits, description, scope?}
```

Announces a handler to the bus. Carries the full `HandlerSpec` shape
plus the optional `scope` (Phase 4c). Until a `HandlerRegistered` has
been seen for `name`, no `Subscribed{handler: name, ...}` can take effect.

```
Subscribed{handler, event_type, filter?}
```

Binds a handler to an event type. `filter` is an optional payload-shape
predicate (Phase 4c+; today the bus matches on event type only).
`Subscribed` is the unit of subscription change — multiple subscriptions
per handler produce multiple `Subscribed` events.

```
Unsubscribed{handler, event_type}
```

Removes a binding. Idempotent — removing a non-existent binding is a
no-op, not an error.

```
HandlerDeregistered{name}
```

Removes the handler and all its bindings. Equivalent to one
`HandlerDeregistered` plus N `Unsubscribed` events; the bus emits the
expansion so the trace records the full picture.

All four are terminal. They describe a control-plane mutation; the
mutation has happened by the time the event is on the log. Anyone who
cares about the change subscribes to the relevant control-plane event.

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
table:

1. A handler subscribes to `Subscribed` and `Unsubscribed`.
2. On each event, it recomputes the SCC over the current live table.
3. If a new SCC has appeared without a `loop:` cap declared on any node,
   it emits `CycleDetected{handlers, event_types, no_cap: true}`.
4. The permission enforcer (if configured) treats `CycleDetected` as a
   policy violation and emits `PermissionDenied` for the offending
   `Subscribed` — which the originating handler can react to.

There is no "static" check distinct from a "runtime" check. The static
graph is the live table at startup, immediately after the seeded
control-plane events have been processed.

This composes cleanly with the `Subscribed`-emitted-from-config flow:
the cycle detector observes the same events whether they came from YAML
seeding or from a Phase 6 optimisation pass.

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

The audit logger:

```yaml
- name: audit
  type: audit
  on: [HandlerRegistered, HandlerDeregistered, Subscribed, Unsubscribed,
       PermissionGranted, PermissionRevoked, PermissionDenied]
  config:
    sink: file:///var/log/reflex-audit.jsonl
```

It is an ordinary handler with no privileged access. It subscribes to
every control-plane and permission event and writes them to an
append-only sink. The audit log is itself an event projection — losing
it cannot disagree with reality because the bus log is reality.

Auditors are not built into the framework; they are configured. A
deployment that doesn't want audit doesn't declare one. A deployment
that wants multiple audit destinations (file + S3 + syslog) declares
three.

## Permission enforcement as a handler

The permission enforcer is also a subscriber. It reacts to every event
that mutates state and checks the principal against the scope grants:

- On `Subscribed{handler, event_type}`: is the principal allowed to
  bind `handler` to `event_type`? If not, emit
  `PermissionDenied{principal, op: "subscribe", scope, reason}`.
- On `Unsubscribed`: same check for `op: "unsubscribe"`.
- On `HandlerRegistered`: is the principal allowed to register a
  handler in the declared `scope`?
- On `PermissionGranted`: was the granter allowed to grant under
  `meta.grant: [scope]`?

The enforcer is a handler with the broadest possible subscription set —
but it has no special bus access. It reads the same log every handler
reads. Its denials are events on that log, observable by other handlers
(the offending handler can react and back off; the audit logger
records the denial; a dashboard subscriber tallies them).

See [`06-permissions-and-scopes.md`](./06-permissions-and-scopes.md).

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
