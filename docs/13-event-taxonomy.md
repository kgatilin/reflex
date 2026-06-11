# 13 — Event taxonomy: subjects, scope, and the trace envelope

The wire shape of an event. Builds on the subject-style types of
[11-domain-model.md](./11-domain-model.md): a subject carries *scope* and
*kind*; a trace envelope carries *correlation* and *causation*; the payload
carries *data*. Every projection is a wildcard subscription over subjects;
every "which request" question is a filter over the trace. This document is
normative; the deltas it implies for current code close it.

## The envelope

```
subject   app.session.<id>.<kind...>     scope (routing + projection bucket)
trace     { request_id?, caused_by[] }   correlation + causation
terminal  bool                           leaf of the causal DAG (see 02/11)
payload   { ...data, source }            data + origin metadata
```

Three axes, three homes. Mixing them is the bug this document removes: the
current `Event` puts kind in `Type`, correlation in `RequestID`, origin in
`Source`, and causation in a scalar `CausedBy` — four flat fields with no
hierarchy and no shared invariant.

## Subjects: NATS grammar, scope prefix, kind suffix

Subjects are `.`-delimited tokens with NATS wildcard semantics: `*` matches
exactly one token in any position; `>` matches one or more tokens, tail only.
Routing lives entirely in the subject; subscription matching is the router
(11-domain-model.md). A subject is `<class>.<scope...>.<kind...>`:

```
sys.<kind...>                    control plane + global registry
app.session.<id>.<kind...>       domain events, scoped by session
app.ingress.<surface>.<event>    inbound traffic before session resolution
```

### Class — machinery vs conversation

The first token is a logical class, and the rule for placing an event is:
**belongs to a session → `app.session.<id>.`; global machinery → `sys.`.**

- `sys.*` — `handler.registered`, `subscribed`, `clock.tick`, and the global
  session registry (thread→session bindings). Session-less infrastructure.
- `app.*` — the domain: requests, llm turns, tool calls, state, scope
  lifecycle. A `scope.closed` for a specific cone is
  `app.session.<id>.scope.closed` — it is about that session.

A monitor watches `app.>` to see all domain activity without machinery noise,
or `sys.>` for the runtime itself.

### Scope is a named axis, not part of the id

The literal `session` token labels the scope axis, leaving room for sibling
scopes under `app.` (`ingress`, and later e.g. `batch.<id>`, `cron.<id>`)
without breaking any wildcard. **Session is the only correlation scope in the
subject.** Request membership is *not* a subject token — it rides in the trace
(below), because a session contains many requests and request id is a
consumer/projection concern, not a routing one.

### Kind — the type taxonomy

The kind suffix is scope-independent and stable, so a projection can address a
kind across all sessions:

```
request.received / request.handled / request.unhandled
llm.turn / llm.completed
tool.<name>.call / tool.<name>.result / tool.<name>.failed
state.updated
scope.opened / scope.closed / scope.deadline_reached
```

The PascalCase legacy types (`RequestReceived`, `ToolCallProposed`,
`AssistantMessageProposed`, `LoopExhausted`, …) retire into this form; delta
#5 of 11-domain-model.md, now a precondition rather than an aspiration —
half-migrated types leave events unreachable by topic.

### Handler desugar

Handlers route on kind, never on scope. The YAML `on:` declares a kind; the
bus prepends the scope wildcard for the subscription:

```yaml
on: tool.calc.call        # subscribes to app.session.*.tool.calc.call
```

Handlers speak pure kind; the bus manages scope. Projections do the opposite —
they bind the scope and wildcard the kind.

## Trace: correlation and causation

Two distinct ids travel with every event:

- **`request_id` (correlation)** — minted on `request.received`, propagated
  unchanged to every descendant. Constant across one request. The flat,
  `O(1)`-filterable answer to "which request am I under".
- **`caused_by[]` (causation)** — the ids of the events that directly caused
  this one. Changes every hop. The causal edge of the DAG; a list, because
  join nodes have N causes (delta #1 of 11-domain-model.md).

`request_id` is *denormalised causation*: the request root you would reach by
walking `caused_by` to its origin, cached flat so per-request filtering does
not require a DAG traversal.

### Stamping rule: narrowest scope covering all causes

The dispatcher — the sole writer of `request_id` — sets it deterministically:

- `request.received` **mints** a fresh `request_id` (start of a request scope).
- an event whose `caused_by` parents all share one `request_id` **inherits**
  it (**request-scoped**).
- an event whose parents span different requests (or that is a non-request
  root) gets an **empty** `request_id` — it lives only at session level
  (**session-scoped**).

A cross-request join therefore resolves itself: an aggregator folding events
from two requests produces a session-scoped event. Nobody "wins" the
correlation; the event rises to the session because, by construction, it is
wider than any one request. Empty `request_id` is not lost information — it is
the correct scope.

Because the rule is purely a function of `caused_by`, `request_id` stays
recomputable from the log; the dispatcher only caches it. The
recompute-from-log invariant (11-domain-model.md) holds, and there is nothing
for a denormalised field to drift from — no handler ever writes it.

### Scoped is a derived property

"Request-scoped" vs "session-scoped" is read off the trace, not stored as a
flag: an event carries a `request_id` ⇔ it is request-scoped. Projections read
both kinds uniformly — `app.session.<id>.>` is the session, an optional
`request_id == R` filter narrows to one request, and session-scoped events
(aggregates, thread summaries) simply pass the "no request" filter.

```
app.session.S.>                          one session (the whole thread)
app.session.S.>  filter request_id == R   one request within it
app.session.*.tool.calc.result            all calc results, every session
app.session.*.llm.completed               every model call (metrics/billing)
app.ingress.>                             the inbound stream before resolution
```

## Session resolution: ingress → request

Origin (Slack, ticket, alert) is *payload metadata*, never a scope axis — the
same session can be fed from several surfaces. An adapter emits a raw,
session-less ingress event; a resolver maps it to a session and only then
emits a scoped `request.received`:

```
app.ingress.slack.message{ thread: "thread-9", text: "..." }
        │  session-resolver reads the sys session registry
        ▼
app.session.S.request.received{ text, source: "slack", thread: "thread-9" }
```

The resolver reads the binding directory, reuses the session if the key is
known, otherwise mints a new session id and emits a fresh binding. `source` and
the surface key land in the payload.

### The registry is the existing state projection

Thread→session resolution needs an index, not a linear fold — but this is the
`state.updated` projection of 11-domain-model.md, which *is* a path→value map
materialised by folding deltas in log order (a last-value-wins KV, exactly like
a NATS KV bucket = a compacted stream). It needs no new concept:

```
sys.state.updated{ path: "session.binding.slack.thread-9", value: "S" }
```

It lives in `sys` because it is the cross-session registry, not part of any one
conversation. One index read at reaction time, still log-derived; the invariant
survives.

## Where the taxonomy pays off

### Cross-session tool-call cache

Because the kind suffix is scope-independent, a tool node can project over
*all* sessions before executing:

```
app.session.*.tool.calc.result   where payload.args == <current args>
```

A hit is recorded as a fresh `app.session.S.tool.calc.result` in the current
cone (`caused_by` → the current call), with the origin referenced in the
payload as a trace annotation:

```json
{ "result": 161, "cache": { "hit": true, "origin_event": "evt-77", "origin_request": "R-old" } }
```

Causality stays local (the `caused_by` edge is in this cone); the cross-session
pointer is payload, not structure.

### LLM node projection

The LLM request is `projection(log) + node config` and nothing else — the node
holds no agent state (the pure fold→complete→emit of 12-react-experiment.md):

| Part | Source | Derived / Config |
|---|---|---|
| `messages` | conversation history `fold(app.session.S.*)` + in-flight `fold(cone)` | derived (log) |
| `tools` | consumers of `app.session.*.tool.*.call` reachable here → schemas | derived (subscription table) |
| `system` | base prompt (+ optional injected scope facts) | config |
| `params` | model, temperature, max_tokens | config |

`messages` draws from both scopes: thread memory is the session subject fold
(prior settled turns), the current step is the causal cone. The two coincide
when a session serialises its requests; they diverge — and the cone becomes
load-bearing — only under concurrent requests within one session (the
cross-request bleed of 12-react-experiment.md F5) or when prior turns are
compacted while the current turn keeps its full tool trace.

## Deltas this implies in current code

1. `Type` → `subject` (`<class>.<scope...>.<kind...>`); PascalCase types
   retire; the matcher gains NATS `*`/`>` semantics; handler `on:` desugars to
   a scope-wildcarded subscription. Current matching is exact-only
   (`pkg/handler/handler.go`, `ev.Type == g.on`).
2. `RequestID` and `Source` flat fields retire: correlation moves into
   `trace.request_id` (dispatcher-stamped, derivable), origin into
   `payload.source`.
3. `CausedBy` scalar → `caused_by[]` list (also delta #1 of 11).
4. A session resolver and the `app.ingress.<surface>` zone are new; the
   session registry is a `sys.state.updated` projection, no new mechanism.

## Open question

Whether the resolver auto-mints a session on every new ingress key or honours
an explicit session start decides who owns the binding key — adapter or
resolver. A Slack thread auto-mints (thread id *is* the key). An alert/cron
surface may want grouping (e.g. by incident id) rather than one session per
event; there the adapter supplies the key and the resolver only binds it.
