# 06 — Permissions and scopes

Phase 4c. Each handler declares a scope. The permission enforcer is a
handler subscribed to control-plane events that emits `PermissionDenied`
when an operation falls outside the principal's grants. Scope hierarchy
is recursive — granting itself is governed by `meta.grant`. Rogue
handlers are contained by observation, not by termination.

## The shape

Every handler declares its `scope` in the same YAML stanza that registers
it:

```yaml
- name: archmotif_analyzer
  type: archmotif_subscriber
  on: [EventDispatched, HandlerRegistered, Subscribed, Unsubscribed,
       HandlerFailed]
  scope:
    mutate:    [analytics.*, triage.*]
    read:      [*]
    forbidden: [core.*, system.*]
    meta:
      grant:   [analytics.*]
```

`scope` has four axes:

- `mutate` — list of scope globs the handler is allowed to publish
  control-plane events for (`HandlerRegistered`, `Subscribed`,
  `Unsubscribed`, `HandlerDeregistered`) when those events target
  resources in the listed scopes.
- `read` — list of scope globs the handler is allowed to receive events
  from. Receivers that don't match are filtered before delivery.
- `forbidden` — explicit deny list; overrides `mutate` / `read` /
  `meta.grant`. Used to keep handlers out of `core.*` / `system.*` even
  when a broader glob like `*` would otherwise admit them.
- `meta.grant` — list of scope globs the handler is allowed to issue
  `PermissionGranted` events for. Default: empty (cannot grant).

The grant axis is what makes the permission model recursive. A handler
that has `meta.grant: [analytics.*]` can mint a `PermissionGranted` for
another handler under `analytics.*` — but not under `core.*`. Root
configuration (the bus bootstrap) has `meta.grant: [*]` and is the only
principal that can hand out broad scopes.

## Scope namespace convention

Scopes are dotted strings: `triage.classifier`, `chat.responder`,
`analytics.metrics`, `core.dispatcher`, `system.bus`,
`feedback.guidance`. Three top-level zones are reserved by the
framework:

- `core.*` — built-in handlers that wire the basic event flow (printer,
  terminator, unhandled_watcher, the canonical chat / triage pipeline).
  Modifiable only with explicit root grant.
- `system.*` — bus internals (`system.bus`, `system.dispatcher`,
  `system.audit`). Modification requires a root grant and is intended
  to be exceptional.
- `feedback.*` — human-owned rules. Reserved for human-added handlers;
  the optimiser cannot touch them without an explicit
  `SuggestedRewrite` + human-gate event chain (see
  [`08-optimization-as-rewrite.md`](./08-optimization-as-rewrite.md)).

User-defined scopes use any other namespace. Glob matching is
left-to-right: `triage.*` matches `triage.classifier`,
`triage.aggregator`, but not `analytics.metrics`.

## Permission events

```
PermissionGranted{principal, scope, ops}
```

Issued by a handler whose own `meta.grant` admits `scope`. `principal`
is the handler name receiving the grant; `scope` is the scope glob
granted; `ops` is the subset of `{mutate, read, forbidden, meta.grant}`
being granted.

```
PermissionRevoked{principal, scope, ops}
```

Reverses a previous grant. Same authority check as
`PermissionGranted`.

```
PermissionDenied{principal, op, scope, reason, event_id}
```

Emitted by the enforcer when a check fails. `event_id` references the
event that triggered the check (typically a `Subscribed`,
`Unsubscribed`, or `HandlerRegistered`) so the offending handler can
correlate the denial with its attempt. The denial does not undo the
attempted operation — by the time the enforcer fires, the event is
already on the log. The enforcer's job is to observe and signal, not to
roll back.

All three are terminal. They describe a policy state, not a unit of
domain work.

## The enforcer

The enforcer is a handler subscribed to every control-plane event:

```yaml
- name: enforcer
  type: permission_enforcer
  on: [HandlerRegistered, Subscribed, Unsubscribed, HandlerDeregistered,
       PermissionGranted, PermissionRevoked]
  scope:
    mutate: [system.permissions]
    read:   [*]
    meta:
      grant: []      # enforcer cannot grant; it only checks
```

On each control-plane event, the enforcer:

1. Identifies the principal (the `source` of the event, typically the
   handler that emitted it).
2. Identifies the target scope of the operation (e.g. for
   `Subscribed{handler, event_type}`, the scope of the handler being
   subscribed and the scope of the event type).
3. Computes the grant set for the principal from the
   `PermissionGranted` / `PermissionRevoked` history.
4. Checks (target scope) against the grant set, with `forbidden`
   overriding everything.
5. If the check fails, emits `PermissionDenied{principal, op, scope,
   reason, event_id: <triggering event>}`.

The enforcer is a normal handler. It reads the same log everyone reads.
Its outputs are events on the same log. There is no privileged
"interceptor" hook; the bus dispatches every event uniformly and the
enforcer runs after the fact.

## Containing rogue handlers

Because the enforcer observes rather than intercepts, the rogue handler
gets to *do* the thing it attempted — the `Subscribed` event is on the
log; the live table updates. The denial fires afterwards.

This is intentional. Three properties fall out of it:

1. **The audit log is complete.** Every attempt — successful or denied —
   is on the log. There is no "secret history" of attempts the enforcer
   blocked.
2. **The offender can react.** A handler that subscribes too broadly
   sees `PermissionDenied{event_id: <its Subscribed>}` and can emit an
   `Unsubscribed` to back off.
3. **No crash mode.** A denial doesn't terminate the run. The system
   keeps going; the offender's next attempt produces another denial.

A persistent offender is a policy problem, not a bus problem. A
follow-up handler (e.g. a "rogue tally" subscriber to `PermissionDenied`)
can take action — emit `HandlerDeregistered` for the offender, or
escalate to a human-gate event for review.

## Recursive grant

`PermissionGranted` is itself a control-plane event. The enforcer
treats it as such:

- The principal of a `PermissionGranted` is the handler issuing the
  grant.
- The target scope is the scope being granted.
- The check: is the principal's `meta.grant` set a superset of the
  granted scope?

A handler with `meta.grant: [triage.*]` can issue
`PermissionGranted{principal: archmotif_analyzer, scope: triage.*,
ops: [mutate]}` — but not `scope: analytics.*`. The enforcer catches the
latter and emits `PermissionDenied`.

Root configuration (the bus bootstrap) is the only principal with
`meta.grant: [*]`. Every other principal's grant set is a subset of
what root handed it. The grant tree is finite, observable, and
auditable.

## Scope hierarchy diagram

```
                          ┌─────────────────┐
                          │   root (boot)   │
                          │ meta.grant: [*] │
                          └─────────┬───────┘
                                    │ PermissionGranted
              ┌─────────────────────┼─────────────────────┐
              ▼                     ▼                     ▼
        ┌──────────┐          ┌──────────┐          ┌──────────┐
        │ enforcer │          │  audit   │          │ archmotif│
        │mutate:   │          │read: [*] │          │mutate:   │
        │ system.* │          │meta.grant│          │ analytics│
        │meta.grant│          │ : []     │          │  .*      │
        │ : []     │          └──────────┘          │meta.grant│
        └──────────┘                                │ : [analy │
                                                    │  tics.*] │
                                                    └────┬─────┘
                                                         │
                                                         ▼
                                                    PermissionGranted
                                                    (to sub-handlers
                                                     under analytics.*)
```

The enforcer can mutate `system.*` (where its own state lives) but
cannot grant anything to anyone. The audit logger reads everything but
cannot mutate or grant. The archmotif analyzer can mutate `analytics.*`
and grant sub-handlers under `analytics.*` — recursive scope, bounded
by `meta.grant`.

## Default protected zones

A canonical bootstrap config establishes:

```yaml
# core.* — the basic event flow, hands-off by default
- PermissionGranted{principal: __all__, scope: core.*, ops: [forbidden]}

# system.* — bus internals
- PermissionGranted{principal: __all__, scope: system.*, ops: [forbidden]}
- PermissionGranted{principal: enforcer, scope: system.permissions, ops: [mutate]}
- PermissionGranted{principal: audit, scope: system.audit, ops: [mutate]}

# feedback.* — human-owned, optimiser can read but not mutate without
# an explicit human-gate
- PermissionGranted{principal: __all__, scope: feedback.*, ops: [read]}
- PermissionGranted{principal: __all__, scope: feedback.*, ops: [forbidden]}
   # forbidden mutates; read is granted explicitly above
```

`__all__` is shorthand for "the default unless a more specific grant
overrides it". The enforcer reads grants in order: the most specific
match wins; `forbidden` overrides all others at the same specificity.

## Filter delivery

Phase 4c extends `Subscribed` with an optional `filter`:

```
Subscribed{handler: T, event_type: X, filter: "payload.user_id == self"}
```

Filters are predicate strings the bus evaluates before delivery. A
handler whose `scope.read` does not admit the source of an event sees
nothing — the event is filtered at the dispatcher edge.

Filters also enable scope-aware routing for control-plane events: the
audit logger receives every event, but a handler with `read:
[analytics.*]` receives only events whose `source` falls in
`analytics.*`. This is the same primitive that makes the enforcer
viable — its `read: [*]` is what lets it see everything.

## Why permissions are a handler, not a primitive

A bus that enforces permissions at dispatch time has two costs:

1. **Coupling.** Permission rules live inside the dispatcher; rules
   change requires bus changes.
2. **Opacity.** Denials are dispatcher-internal; nothing on the log
   records them.

Reflex's approach inverts both. Permission rules live in the
`PermissionGranted` / `PermissionRevoked` event stream — addable,
removable, auditable at runtime. Denials are events on the log —
visible to handlers, surfaceable in the trace, queryable by the
analyzer.

The dispatcher does not know about permissions. The bus accepts every
event, fans it out per the live table, and lets the enforcer decide
afterwards what was a violation. The cost is that the offending event
is briefly on the log; the benefit is that the system can observe its
own policy state and reason about it the same way it reasons about
domain state.
