# reflex

> event-sourcing agent PoC: no loop, no agent — events + YAML-declared subscribers + projection state.

Full design documentation lives in [`docs/`](docs/README.md). Start with
[`docs/01-mental-model.md`](docs/01-mental-model.md); for the externally-facing
API see [`docs/09-embedding-api.md`](docs/09-embedding-api.md); for phase
status see [`docs/10-phase-roadmap.md`](docs/10-phase-roadmap.md).

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

reflex's verbs, all driven by `--config <yaml>`:

```
reflex run         --config <yaml> --message <text> [--trace] [--wait <p>]
reflex emit        --config <yaml> --type <Type> --payload <json> [--wait <p>]
reflex invoke      --config <yaml> <command> [args]    [--wait <p>]
reflex send        --config <yaml> <text>              [--wait <p>]
reflex validate    --config <yaml>
reflex describe    --config <yaml>
reflex new-handler <name> --consumes <Type> [--emits ...] [--terminal ...] [--scope ...] [--language yaml|go]
```

- `run` — execute a single user message through the bus and print the trace.
- `emit` — seed an arbitrary event into the bus (any type, any payload).
- `invoke` / `send` — Phase 1.6 sugar over `emit` using the YAML
  `events.cli` bindings (see Phase 1.6 section above).
- `--wait <predicate>` — block until a post-drain predicate is satisfied:
  `drain` | `request_id_terminal` | `projection.has=<key>`.
- `validate` — compile the YAML into a static handler graph and check for
  uncapped cycles. Exits 0 on a valid config and prints
  `config valid: N handlers, M edges, K declared loops`. Exits 1 on an
  uncapped cycle and prints the cycle trace.
- `describe` — print the handler graph as a textual table: name, type,
  description, consumes, emits (terminal emissions tagged `(T)`), and the
  declared loop cap if any.
- `new-handler` — scaffold a new handler. With `--language yaml` (default)
  appends a handler block to `--config` (or prints to stdout). With
  `--language go` writes a runnable handler binary at `cmd/<name>/main.go`
  using `pkg/sdk`. Refuses to overwrite. See
  [`docs/02-handlers-and-schemas.md`](./docs/02-handlers-and-schemas.md) →
  "Scaffolding".

```
reflex validate --config examples/aggregate.yaml
# config valid: N handlers, M edges, 0 declared loops

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

## Phase 1.6: meta-events, projection, aggregator, wait-predicates

Reflex's core thesis is **everything is an event**. There is no synchronous
wait-for-reply primitive — no `hook`, no RPC-shaped addressing. Phase 1.6
delivers the pieces that let real fan-out / barrier patterns be expressed
without breaking that invariant.

### Bus meta-events

The bus publishes its own activity onto the same log it routes user
events on. All three meta-events are terminal (they describe a routing
step; they don't trigger further user work) and carry a `caused_by`
linking back to the event they describe:

- `EventDispatched{event_type, subscriber_count}` — emitted after the bus
  has finished delivering an event to all matching subscribers.
  `subscriber_count` is the number of handlers that matched.
- `DrainQuiesced{request_id}` — emitted when no work remains for a
  `request_id`. One per request_id observed during the run.
- `HandlerFailed{handler_name, event_type, error}` — emitted when a
  handler's `React` raises.

Meta-events are first-class. Handlers can subscribe to them
(`on: EventDispatched`), the analyzer reads them, the CLI wait-predicates
key off them. The bus never emits a meta-event about another meta-event
(that would recurse). The orphan watcher specifically ignores
`EventDispatched` when counting descendants, so the Phase 1 invariant
stays meaningful.

### Projection as a first-class side-channel

`SessionProjection` (the pure fold) is the canonical state derivation, but
some patterns want a place to stash a structured intermediate result that
downstream handlers can pick up by key — a classifier verdict, an extracted
entity, a parsed plan. `pkg/projection.Store` is that side-channel: a
per-request key/value map that the runtime wires into every handler
implementing `bus.ProjectionAware`. Handlers `Set(request_id, key, value)`
during reaction; downstream readers `Get(request_id, key)`.

The projection is not a substitute for events — anything that should
affect causal structure stays an event. It is a way to express "I have
decided X" once, without re-emitting an event every time someone wants to
know. A classifier handler can write its decision under `calc.verdict`; a
CLI predicate `projection.has=calc.verdict` then waits for it.

### Generic aggregator handler

The `aggregator` handler type collects N responses to a fan-out trigger
and emits one aggregated event once enough have arrived. Crucially, it
does not need to know N at construction time — it learns it from
`EventDispatched.subscriber_count` for the fan-out event:

```yaml
- name: collect
  kind: aggregator        # YAML alias: type: aggregator
  on: Classification      # the per-handler response events to accumulate
  emits: [ClassificationsAggregated]
  config:
    expected_from: ClassifyRequested   # take subscriber_count from EventDispatched of this type
    emit: ClassificationsAggregated    # the aggregated event type
```

When the aggregator has seen `subscriber_count` responses for the same
`request_id`, it emits a terminal
`ClassificationsAggregated{items: [...], count: N}` event. Exactly once
per request. `examples/aggregate.yaml` is a 3-classifier fan-out with the
aggregator finalising into `RequestHandled`.

### CLI: emit, invoke, send + wait-predicates

The `reflex run --message <text>` shorthand still works. Phase 1.6 adds:

- `reflex emit --config <yaml> --type <EventType> --payload <json>` —
  seed an arbitrary event into the bus.
- `reflex invoke <command> [args]` — sugar over `emit` using YAML-declared
  bindings.
- `reflex send <text>` — even more sugar: emits the event whose
  `cli.command: send` binding matches.

All three accept `--wait <predicate>`. Three predicates ship:

- `drain` — succeed once `DrainQuiesced` fires for the request_id.
- `request_id_terminal` — succeed once any user-domain terminal event
  (RequestHandled, RequestUnhandled, EventOrphaned, LoopExhausted, etc;
  meta-events don't count) fires for the request_id.
- `projection.has=<key>` — succeed once the projection store contains
  `key` for the request_id.

Events can declare their default CLI binding + wait predicate in the YAML:

```yaml
events:
  - name: ClassifyRequested
    args: { item: string }
    cli:
      command: invoke classify
      wait: drain

  - name: MessageReceived
    args: { text: string }
    cli:
      command: send
      wait: drain
```

So `reflex invoke classify foo` is sugar for
`reflex emit --type ClassifyRequested --payload '{"item":"foo"}' --wait drain`.

## Phase 4a: daemon mode + remote handler SDK

The in-process model (`reflex run`, `reflex emit --config …`) is still the
default. Phase 4a adds an alternative deployment shape: a long-running
**daemon** process owns the bus, and external **handler clients** connect to
it over a Unix domain socket. Handlers can live in any Go process — the SDK
in `pkg/sdk/` exposes the same semantics a YAML-declared handler gets
(consumes / emits / terminal-marking / projection access).

This is the foundation for later phases: handlers running in their own
process; cross-language handlers; bus subscribers that are not "agents" at
all (metrics, audit, replay).

### Anatomy

- `reflex daemon --config <yaml> [--socket <path>]` — starts the bus, loads
  YAML handlers, listens on a Unix socket. Default socket is
  `${XDG_RUNTIME_DIR:-/tmp}/reflex.sock`. Graceful shutdown on
  SIGINT/SIGTERM.
- `pkg/sdk` — Go SDK. `sdk.Connect(sdk.Remote(socket))` opens a connection;
  `sdk.NewHandler(...)` declares the subscription; `client.Register(handler)`
  installs it; `client.Run(ctx)` blocks until ctx is cancelled or the daemon
  goes away.
- `reflex emit --type X --payload '{...}' --daemon <socket>` — sends the
  seed event to a running daemon instead of running in-process. `invoke` and
  `send` accept `--daemon` too.

### Minimal SDK handler

```go
client, _ := sdk.Connect(sdk.Remote("/tmp/reflex.sock"))
defer client.Close()

h := sdk.NewHandler("my-handler",
    sdk.Consumes("MessageReceived"),
    sdk.Emits("ResponseEmitted"),
    sdk.Terminal("ResponseEmitted"),
).OnEvent(func(ctx sdk.Ctx, ev sdk.Event) error {
    return ctx.Emit("ResponseEmitted", sdk.Args{"text": "hi"})
})
client.Register(h)
client.Run(context.Background())   // blocks
```

The same `sdk.NewHandler(...).OnEvent(...)` works in-process via
`sdk.Connect(sdk.InProcess(bus))`; only the transport changes.

### End-to-end example

`examples/calc.yaml` declares the YAML side: the in-process handlers that
own the bus. `cmd/reflex-sample-handler/` is a standalone binary that
registers a handler on `RequestReceived` and emits `ResponseEmitted`.

```
# terminal 1 — daemon
reflex daemon --config examples/calc.yaml --socket /tmp/reflex-demo.sock

# terminal 2 — sample handler
go run ./cmd/reflex-sample-handler --socket /tmp/reflex-demo.sock

# terminal 3 — seed event
reflex emit --type RequestReceived --payload '{"payload":"hello"}' \
    --daemon /tmp/reflex-demo.sock
```

Daemon terminal prints `response: echo: HELLO` (the SDK handler uppercases
the payload; the printer prefixes "response: ").

### Wire protocol

Newline-delimited JSON over a Unix domain socket. One connection can host
N handlers (each via its own `hello`/`welcome` handshake on the same
socket). Message types: `hello` / `welcome` (handshake), `deliver` /
`ack` / `nack` (event delivery + completion — `deliver` carries
`handler_name` for multi-handler demultiplexing), `emit` (handler emits —
tied to a `delivery_id` if mid-delivery, otherwise treated as a fresh
seed), `goodbye`, `error`. Phase 4b adds `await` / `resolved` (wait
predicates over the socket) and `proj_get` / `proj_value` / `proj_set`
(projection store RPCs). See `pkg/sdk/protocol.go` for the full spec.

### + Phase 4b: control-plane events + daemon completeness

Phase 4b builds on 4a:

- The bus emits five control-plane events on every subscription mutation
  (`HandlerRegistered`, `Subscribed`, `Unsubscribed`, `HandlerDeregistered`,
  `SubscriptionRejected`). YAML loading is now a seeded stream of those
  events; the audit handler in `examples/control_plane_audit.yaml`
  subscribes and writes them to a JSONL log.
- The cycle detector runs over the live subscription table on every
  registration. The Phase 1.5 static YAML check stays as a fast
  pre-flight; the live-table check is the authority. A runtime
  `SubscribeWithCheck` that would close an uncapped cycle is refused with
  a `SubscriptionRejected` event.
- All four Phase 4a daemon TODOs are closed: wait-predicates over the
  socket (`--wait drain` / `request_id_terminal` / `projection.has=KEY`
  with `--daemon`), projection RPCs for remote handlers, multi-handler
  mux per connection, and post-drain `CheckQuiescence` so the daemon
  surfaces `RequestUnhandled` / `EventOrphaned` for socket-driven seeds.

### Out of scope here (later sub-phases)

- Permission/scope checks on register (4c).
- Scaffold CLI for new handlers (4d).
- HTTP / Go-embed external API (4e).

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
cmd/reflex/                  CLI entry point (run / emit / invoke / send / validate / describe / daemon)
cmd/reflex-sample-handler/   Phase 4a end-to-end example: SDK handler in its own process
pkg/event/                   Event type + in-memory append-only store
pkg/bus/                     dispatcher (drain function, not goroutine pool) + Subscriber interface + loop cap enforcement
pkg/projection/              SessionProjection — pure fold of events for one request
pkg/handler/                 built-in handler factories + HandlerSpec self-description + Introspect registry projection
pkg/graph/                   static handler graph compiler + Tarjan SCC cycle detection
pkg/config/                  YAML loader + validation (including `loop:` grammar)
pkg/sdk/                     Phase 4a: handler-client SDK (in-process + Unix-socket transports), daemon, wire protocol
internal/runtime/            glue: build a bus from a config, run a single user message
examples/                    calc / stall / aggregate / react + loop (capped cycle) + bad_loop / cycle (validate negatives) + control_plane_audit / scoped_compression (Phase 4b/4c)
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
| `aggregator`         | configurable (the response type) | event type from `config.emit` (terminal) | `expected_from` (fan-out event type), `emit` (aggregated type) |

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
- `aggregator` → its aggregated `config.emit` event is terminal (it closes
  the fan-in barrier once `subscriber_count` responses have arrived).

Custom handlers should default to non-terminal (`event.New(...)`) and
opt into terminal only for genuine leaves (`event.NewTerminal(...)`).

## Tests

```
go test ./...
```

Covers the projection fold, dispatcher fan-out and bounded termination,
YAML parse + validate (including unknown-type rejection), the calc tool,
the printer, the request-unhandled watcher, the event-orphan watcher
(Phase 1 invariant), the static graph compiler + cycle detection, the
aggregator fan-in barrier, the permission/scope layer, and the Phase 4
daemon + handler-client SDK end-to-end.
