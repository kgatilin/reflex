# reflex

> event-sourcing agent PoC: no loop, no agent — events + YAML-declared subscribers + projection state.

## The concept

Most "agents" are a monolithic `prepare → think → act → observe` loop. reflex
inverts that. There is no agent loop, no orchestrator. Everything that
happens is an **event** on an append-only log. Behaviors are **subscribers**:
small reactive functions that match an event type, project the state they
need from the log, and emit zero or more new events. The LLM is one such
subscriber. Tools are subscribers. The printer is a subscriber. The
"super-agent" that emerges from `event → subscriber → new event → ...` is
just the transitive closure of those reactions — it terminates when the
queue is empty.

Session state is **never stored**. When a subscriber needs to know what has
happened so far, it calls `SessionProjection(events, request_id)` — a pure
fold. Losing the projection cannot disagree with reality because the log is
reality. To close the loop, every `RequestReceived` event must eventually
produce a `RequestHandled` event; a post-drain watcher emits
`RequestUnhandled` if quiescence is reached without it, so unresolved
requests are observable rather than silent.

## Quickstart

```
go run ./cmd/reflex run --config examples/calc.yaml --message "what is 2+2"
# assistant: The answer is 4
```

Add `--trace` to dump the full event log as one JSON object per line:

```
go run ./cmd/reflex run --config examples/calc.yaml --message "what is 2+2" --trace
```

Try the deliberately-broken stall example to see the watcher fire:

```
go run ./cmd/reflex run --config examples/stall.yaml --message "anything" --trace
# assistant (stall): I will speak, but I will never close the request.
# ...trace lines including a RequestUnhandled event...
# request <uuid> unhandled: drain quiesced without RequestHandled
# exit status 2
```

## Subcommands

reflex has three verbs, all driven by `--config <yaml>`:

```
reflex run      --config <yaml> --message <text> [--trace]
reflex validate --config <yaml>
reflex describe --config <yaml>
```

- `run` — execute a single user message through the bus and print the trace.
- `validate` — compile the YAML into a static handler graph and check for
  uncapped cycles. Exits 0 on a valid config and prints
  `config valid: N handlers, M edges, K declared loops`. Exits 1 on an
  uncapped cycle and prints the cycle trace.
- `describe` — print the handler graph as a textual table: name, type,
  description, consumes, emits (terminal emissions tagged `(T)`), and the
  declared loop cap if any.

```
reflex validate --config examples/triage.yaml
# config valid: 7 handlers, 6 edges, 0 declared loops

reflex validate --config examples/cycle.yaml
# cycle detected: alpha -> beta -> gamma -> alpha; no max_iterations declared; refusing to start
# exit 1
```

## Handlers are nodes in a graph

From Phase 1.5 on, **handlers are self-describing nodes in a graph from the
start**. Every registered handler type ships with a `HandlerSpec` declaring
its `Consumes` event type and its `Emits` set (each with `Terminal` /
`Optional` flags). The `pkg/handler` registry exposes a read-only
`Introspect` projection — `ListTypes`, `SpecOf`, `Emitters`, `Consumers` —
that downstream packages use to reason about the handler topology without
instantiating a single handler.

`pkg/graph.Build` compiles a YAML config into a `HandlerGraph` via that
projection: one node per declared handler, one edge per `(emitter, event
type, consumer)` triple. Tarjan SCC is run over the result; any cycle that
isn't capped by an explicit `loop: { max_iterations: N }` declaration is a
hard error and the runtime refuses to start. This makes graph-shape
validation a load-time concern rather than a runtime surprise.

This is the foundation Phase 4 (the daemon + client SDK) will lean on:
handlers in separate processes will announce themselves to a bus daemon
using exactly this `HandlerSpec` shape, and the daemon will run the same
graph validation before letting traffic flow.

## Loops

Cycles are sometimes the right shape — a reviewer LLM loop, a polling
fetcher, an iterative refinement chain. Reflex models them explicitly: one
node in the cycle must declare `loop: { max_iterations: N }`. The static
graph validator accepts the cycle, and the dispatcher enforces the cap at
runtime per `(request_id, handler_name)`. When the cap is hit, the
dispatcher emits a terminal `LoopExhausted{handler, max_iterations, reason}`
event instead of firing the handler again — the request closes cleanly.

```yaml
handlers:
  - name: bouncer
    type: echo
    on: PongEvent
    config: { emit: PingEvent }
    loop:
      max_iterations: 2     # bouncer fires at most twice per request
  - name: pongbacker
    type: echo
    on: PingEvent
    config: { emit: PongEvent }
```

`examples/loop.yaml` ships a capped 2-handler loop you can run end-to-end;
`examples/bad_loop.yaml` is the same topology without a cap and
`examples/cycle.yaml` is a 3-node uncapped cycle — both are intended for
demonstrating `reflex validate` refusing to start.

## Repo layout

```
cmd/reflex/         CLI entry point (run / validate / describe subcommands)
pkg/event/          Event type + in-memory append-only store
pkg/bus/            dispatcher (drain function, not goroutine pool) + Subscriber interface + loop cap enforcement
pkg/projection/     SessionProjection — pure fold of events for one request
pkg/handler/        built-in handler factories + HandlerSpec self-description + Introspect registry projection
pkg/graph/          static handler graph compiler + Tarjan SCC cycle detection
pkg/config/         YAML loader + validation (including `loop:` grammar)
internal/runtime/   glue: build a bus from a config, run a single user message
examples/           calc / stall / triage (working) + loop (capped cycle) + bad_loop / cycle (validate negatives)
```

## YAML handler grammar

```yaml
settings:
  max_steps: 64           # optional cap on dispatcher iterations

handlers:
  - name: <unique label>  # required, used in the trace
    type: <handler type>  # required, one of the registered types below
    on:   <event type>    # required, the event the handler subscribes to
    emits: [Type, ...]    # informational, helps readers of the YAML
    config: { ... }       # type-specific parameters
    loop:                 # optional, Phase 1.5: declares this handler as
      max_iterations: N   # the cap-bearing node of a cycle
      name: <loop label>  # optional, defaults to the handler name
```

### Handler types

| `type:`              | Subscribes to       | Emits                                      | `config:` fields |
|----------------------|---------------------|--------------------------------------------|------------------|
| `llm_stub`           | configurable        | `ToolCallProposed`, `AssistantMessageProposed`, `RequestHandled` | `rules[]`, `fallback`, `trigger_on[]` |
| `tool_call`          | `ToolCallProposed`  | `ToolResultObserved`                       | `tool` (one of `calc`, `echo`, `length`, `upper`) |
| `printer`            | typically `AssistantMessageProposed` | nothing                  | `prefix`, `field` (default `text`) |
| `terminator`         | configurable        | `RequestHandled` (if not already)          | (none) |
| `unhandled_watcher`  | `__noop__`          | `RequestUnhandled`, `EventOrphaned` (post-drain) | (none) |
| `echo`               | configurable        | event type from `emit:`                    | `emit` |
| `parse_target`       | typically `RequestReceived` | `TargetParsed`, `ParseFailed`       | `default_owner` (default `kgatilin`) |
| `gh_query`           | typically `TargetParsed` | `GhQueryResult`, `GhQueryFailed`       | `path` (e.g. `comments`, `timeline`) |
| `triage_rules`       | typically `GhQueryResult` | `TriageDecided`, `TriagePending`     | `stuck_hours` (48), `kira_login`, `now` (RFC3339, default real `time.Now()`) |

### `llm_stub` rule grammar

```yaml
rules:
  - match: <substring>         # case-insensitive substring of the trigger
    action: tool_call | reply | reply_and_handle | none
    tool:  <tool name>         # required when action=tool_call
    args:  <args string>       # supports {user_message} / {last_tool_result}
    reply: <text>              # supports {user_message} / {last_tool_result}
fallback:                      # used when no rule matches
  action: ...
  ...
trigger_on: [RequestReceived, ToolResultObserved]   # optional override of `on:`
```

A rule's `action` field tells reflex what to emit when the rule fires:

- `tool_call` — emit `ToolCallProposed{tool, args}`.
- `reply` — emit `AssistantMessageProposed{text}` (request stays open).
- `reply_and_handle` — emit `AssistantMessageProposed{text}` **and**
  `RequestHandled{}` (closes the request).
- `none` — emit nothing (used in the stall example).

Templating in `reply` and `args`:
- `{user_message}` — the original user input.
- `{last_tool_result}` — the most recent `ToolResultObserved.result`.

### Stub-LLM matching rules in `examples/calc.yaml`

The shipped calc example matches on these substrings (first match wins):

| Substring in user message | Action                                   |
|---------------------------|------------------------------------------|
| `+`, `-`, `*`, `/`        | call the `calc` tool with the message    |
| `hello`, `hi`             | reply with a help message and handle     |
| anything else             | fallback: "I only know basic arithmetic" |

The `calc` builtin scans its `args` for the first contiguous run of digits
and one operator (`+ - * /`), so `"what is 2+2"`, `"compute 5*3"`, and
`"2+2"` all work.

## Annotated event log (calc demo)

```
{"type":"RequestReceived",         "source":"cli",                 "payload":{"payload":"what is 2+2"}}
{"type":"ToolCallProposed",        "source":"brain-initial",       "payload":{"tool":"calc","args":"what is 2+2"}}
{"type":"ToolResultObserved",      "source":"calc-tool",           "payload":{"result":"4"}}
{"type":"AssistantMessageProposed","source":"brain-after-tool",    "payload":{"text":"The answer is 4"}}
{"type":"RequestHandled",          "source":"brain-after-tool"}
```

Every event carries `id`, `request_id`, `ts`, and (where applicable)
`caused_by` so the causal chain is fully reconstructable from the log.

## Swapping the stub for a real LLM

Implement one Go type that satisfies `bus.Subscriber`:

```go
func (a *anthropicSub) Match(ev event.Event) bool { return ev.Type == "RequestReceived" }
func (a *anthropicSub) React(ctx context.Context, ev event.Event, log []event.Event) ([]event.Event, error) {
    state := projection.SessionProjection(log, ev.RequestID)
    // build messages from state, call the Anthropic SDK, decide which
    // events to emit (tool_call vs assistant message + request_handled).
}
```

Register a factory for it under a new YAML `type:` (e.g. `anthropic`) via
`handler.Registry.Register`, swap `llm_stub` for `anthropic` in your YAML,
done. Nothing else in the pipeline changes — the rest of the system reacts
only to event types.

## Terminal-event invariant

Every event carries a boolean `terminal` field. Non-terminal events are
expected to spawn at least one descendant (an event with `caused_by` ==
their `id`); terminal events are explicit leaves of the causal DAG. The
post-drain check (`CheckQuiescence`) emits `EventOrphaned{orphan_id,
orphan_type, request_id}` (terminal) for every non-terminal event with
zero descendants — a hard architectural-violation diagnostic, distinct
from `RequestUnhandled` (request-level) vs `EventOrphaned` (event-level).

Stock handlers mark events terminal when appropriate:

- `llm_stub` action `reply_and_handle` → both `AssistantMessageProposed`
  and `RequestHandled` are terminal (printer reads AMP but emits nothing,
  so AMP closes the chain).
- `terminator` → `RequestHandled` is terminal.
- `unhandled_watcher` → `RequestUnhandled`, `EventOrphaned` are terminal.
- `parse_target` failure → `ParseFailed` is terminal.
- `gh_query` failure → `GhQueryFailed` is terminal.
- `triage_rules` → `TriageDecided` is non-terminal (printer + terminator
  downstream); `TriagePending` is terminal (covers the "waiting for the
  other path" and "already decided" cases so trigger events still have
  a descendant and stay invariant-compliant).

Custom handlers should default to non-terminal (`event.New(...)`) and
opt into terminal only for genuine leaves (`event.NewTerminal(...)`).

## Triage example

The `examples/triage.yaml` config classifies a real GitHub agent-ready
issue as `STUCK`, `HEALTHY`, or `FRESH` using only the `gh` CLI on PATH:

```
go run ./cmd/reflex --config examples/triage.yaml --message "archai#114"
# triage: label_age=267h, kira=0 → STUCK

go run ./cmd/reflex --config examples/triage.yaml --message "archai#98"
# triage: label_age=730h, kira=1 → HEALTHY

go run ./cmd/reflex --config examples/triage.yaml --message "archai#9999"
# (no triage line — GhQueryFailed + RequestUnhandled, exit 2)
```

Classification rules:

- `STUCK`   — `label_age > 48h` and `kira_interactions == 0`
- `HEALTHY` — `kira_interactions > 0`
- `FRESH`   — `label_age ≤ 48h` and `kira_interactions == 0`

`LABEL_AGE_HOURS` = hours since the most recent `labeled` timeline event
with `label.name == "agent-ready"` (defensive fallback: earliest timeline
entry, since the `/timeline` endpoint always returns the issue's history).

`KIRA_INTERACTIONS` = comments authored by `kira-autonoma` + timeline
`cross-referenced` entries whose `source.issue.user.login == kira-autonoma`.
`mentioned` and `subscribed` timeline events with the same actor are
deliberately **not** counted (they auto-fire on @-mentions and are false
positives).

The triage chain ends in `RequestHandled` on success; the example also
shows the Phase 1 invariant in action — even when only one of the two
`GhQueryResult` paths arrives at the time the dispatcher pops the first
event, `triage_rules` emits a terminal `TriagePending` so the trigger
`GhQueryResult` still has a descendant and the orphan watcher stays
silent.

## Tests

```
go test ./...
```

Covers the projection fold, dispatcher fan-out and bounded termination,
YAML parse + validate (including unknown-type rejection), the calc tool,
the printer, the request-unhandled watcher, the event-orphan watcher
(Phase 1 invariant), `parse_target` / `gh_query` / `triage_rules`
(Phase 2 triage pipeline) with mocked CmdRunner against captured-from-prod
fixtures in `pkg/handler/testdata/`, and three end-to-end runs covering
STUCK / HEALTHY / 404-not-found.
