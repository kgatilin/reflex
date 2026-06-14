# 01 — Mental model

The synthesis. Reflex is an event-sourced agent runtime in which every
component — domain logic, control plane, audit, analytics, human feedback,
permission system — lives as a handler subscribed to events on a single
bus. There are no synchronous primitives; there is no privileged plane.

## Events-only

A handler is, mechanically, a tuple

```
consumes: <event type>
emits:    [<event type>, ...]
```

There is no `hook`, no RPC, no `waitForReply`, no blocking call. There is
no synchronous primitive at any layer. "Send a request and wait for a
reply" decomposes into two events with a shared `request_id` correlation:

```
RequestX{request_id: R, payload: ...}
ResponseY{request_id: R, payload: ..., terminal: true}
```

The party that wants to "wait" does not block. It registers interest in
the projection (see below) under a wait-predicate; when the predicate
holds, the projection resolves and the waiter unblocks. The bus does not
know about waiters.

This is enforced not by convention but by the dispatcher's structure
(`pkg/bus/bus.go`): a subscriber's `React` returns a slice of events
synchronously, and any "waiting" it appears to do is just it not emitting
yet. Nothing in the runtime can block on another event.

## Graph ≡ subscription table

The static handler graph (load-time) and the runtime subscription set are
the same structure. There is no "declared topology" sitting alongside an
"actual topology" that can drift from it. YAML is just one way to seed
the bus: every line of YAML produces a `HandlerRegistered + Subscribed`
event at startup. Once the seed events have been processed the bus holds
the table; the YAML file is irrelevant.

The current implementation (`pkg/graph/graph.go`) compiles the YAML into
a `HandlerGraph` at load time and runs Tarjan SCC over it. This is
adequate for Phase 1.5 — the YAML cannot change at runtime, so the
load-time graph is the runtime graph. Phase 4b promotes registration to
event streams, after which the cycle detector runs over the live
subscription table — same algorithm, different source. See
[`05-control-plane-as-events.md`](./05-control-plane-as-events.md).

## Bus self-hosts

The bus publishes meta-events about its own activity onto the same log
that carries domain events. Three meta-events ship today
(`pkg/bus/bus.go`):

```
EventDispatched{event_type, subscriber_count}    after each fan-out
DrainQuiesced{request_id}                        when no work remains for a request
HandlerFailed{handler_name, event_type, error}   when React errs
```

All three are terminal. They describe a routing step, not a unit of user
work. Handlers can subscribe to them — the generic aggregator
(`pkg/handler/aggregator.go`) relies on `EventDispatched.subscriber_count`
to learn the width of a fan-out and decide when "enough" responses have
arrived. Wait-predicates (`drain`, `request_id_terminal`) read meta-events
too.

The bus refuses to emit a meta-event about another meta-event — that
would recurse. This is the only special case in the otherwise uniform
"everything is an event" rule, and the orphan watcher likewise excludes
`EventDispatched` from descendant counts so the terminal-event invariant
(see `02-handlers-and-schemas.md`) stays meaningful.

Phase 4b extends the catalogue with control-plane meta-events:

```
HandlerRegistered{name, consumes, emits, description}
Subscribed{handler, event_type, filter?}
Unsubscribed{handler, event_type}
```

Phase 4c adds permission meta-events:

```
PermissionGranted{principal, scope, ops}
PermissionRevoked{principal, scope, ops}
PermissionDenied{principal, op, scope, reason}
```

All of these are themselves ordinary terminal events. The bus configures
itself with events about itself.

## Projection-as-truth

The append-only event log is the single source of truth. A subscriber
that needs to know "what has happened so far in this request" does not
read shared memory; it calls `SessionProjection(events, request_id)` —
a pure fold (see `pkg/projection/projection.go`). The projection cannot
disagree with reality because the log is reality.

Some patterns nonetheless want to stash a structured intermediate result
that downstream handlers pick up by key — a classify verdict, an extracted
entity, a parsed plan. The projection store
(`pkg/projection/store.go`) is that side-channel: a per-request key/value
map with `Set(req_id, key, value)` / `Get(req_id, key)` / `Has(req_id,
key)`. The runtime wires it into every subscriber that implements
`bus.ProjectionAware`.

The projection store is not a substitute for events. Anything that
should affect causal structure stays an event. It is a way to express
"I have decided X" once, without re-emitting every time a downstream
reader wants to know.

The CLI's wait-predicates are themselves waiters over the projection:

```
--wait drain                      → DrainQuiesced fired for request_id
--wait request_id_terminal        → any user-domain terminal event fired
                                    (meta-events don't count)
--wait projection.has=calc.verdict     → projection store has key set
```

The waiter is not a privileged construct. It is a post-drain validator.
A future daemon mode (Phase 4a) extends this to mid-drain async
waiting — same predicates, different evaluation site.

## No privileged plane

Every component in the system is a handler with declared `consumes` /
`emits` / (Phase 4c) `scope`:

- The archmotif analyser is a handler that subscribes to control-plane
  meta-events and domain events, maintains the runtime graph as a
  projection, and may propose subscription rewrites.
- The audit logger is a handler subscribed to `Subscribed` / `Unsubscribed`
  / `PermissionGranted` / `PermissionRevoked` that writes an
  append-only record.
- The permission enforcer is a handler that checks every
  control-plane operation against the scope grants and may emit
  `PermissionDenied`.
- The optimiser is a handler that listens for trace events,
  proposes rewrites, and (within its scope) emits subscription
  changes.
- The human gate is a handler that subscribes to "candidate rewrite"
  events, presents them to a human, and emits "accepted" or
  "rejected".

Differences between components are scope, not access. A handler outside
its scope still emits — the enforcer sees the violation and emits
`PermissionDenied`. The offending handler can react to that. Nothing
crashes; the system observes its own misbehaviour and decides what to
do.

## Event flow

A single event flowing through the system, illustrative:

```
   ┌─────────────────────────────────────────────────────────────────┐
   │ Bus (single in-memory store, drain loop)                       │
   │                                                                 │
   │  RequestReceived{request_id:R, payload:"what is 2+2"}           │
   │      │                                                          │
   │      ├─► parse ─► StepParsed{a:2, b:2, op:"+"}                  │
   │      │      │                                                   │
   │      │      ├─► fetch_a ─► StepFetched{path:left}              │
   │      │      └─► fetch_b ─► StepFetched{path:right}             │
   │      │            │                                             │
   │      │            └─► classify ─► StepDecided{result:4}        │
   │      │                  │   │                                   │
   │      │                  │   └─► announce (printer)   [sink]     │
   │      │                  └─► finalize ─► RequestHandled (T)      │
   │      │                                                          │
   │      ├─► EventDispatched{event_type:..., subscriber_count:1}    │
   │      └─► (after queue empties) DrainQuiesced{request_id:R}      │
   │                                                                 │
   │  Projection store: { R: { "calc.verdict": "4" } }               │
   └─────────────────────────────────────────────────────────────────┘
            ▲
            │
       Waiter (CLI):  --wait projection.has=calc.verdict
                      resolves once the key appears.
```

`(T)` marks terminal events. The drain ends when the queue is empty;
`DrainQuiesced` is emitted per `request_id`. The waiter resolves
either before drain (in a daemon mode that supports mid-drain
predicates) or once drain finishes (current CLI semantics).

## The simplification

Reflex's core claim is that one substrate — events on a single bus, with
self-describing handlers, meta-events about the bus, and projections
written and read by subscribers — is enough to express:

- domain agent logic (chat, classify, tools),
- the static topology (`HandlerRegistered`, `Subscribed`),
- the runtime topology's changes (`Unsubscribed`, optimisation rewrites),
- the bus's own activity (`EventDispatched`, `DrainQuiesced`,
  `HandlerFailed`),
- audit (handler subscribed to control-plane events),
- permissions (handler subscribed to control-plane events with
  enforcement),
- human-in-the-loop feedback (handler emits a "candidate" event,
  another emits "accepted"),
- live graph analysis (subscriber maintaining a projection of the
  topology),
- optimisation (subscriber that emits subscription-change events
  rewriting the graph).

Each layer is the same shape. The complexity sits in handler logic, not
in framework primitives. The framework is "publish + subscribe + drain +
projection + meta-events" and nothing else.

The implications for tooling (the `pkg/embed` API, the HTTP daemon, the
Go SDK) are correspondingly small: foreign apps see emit / invoke /
projection-read / subscribe and never touch the bus internals. See
[`09-embedding-api.md`](./09-embedding-api.md).
