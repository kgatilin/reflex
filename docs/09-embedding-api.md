# 09 — Embedding API

Phase 4e. The externally-facing API for foreign applications that want
to use reflex as a component. Two surfaces ship in v1: a Go `pkg/embed`
in-process package, and an HTTP API over the Phase 4a daemon. An
optional gRPC mirror is deferred unless needed. The surface is
deliberately tiny — emit, invoke, projection-read, subscribe — and
hides the bus internals entirely.

## What an embedder sees

A foreign application using reflex sees a reactive service. It posts
events; the agent processes them; the application reads results from a
projection or a stream. It does not see the handler graph, the bus
internals, the compression machinery, or the permission system. The
internal complexity (events-only, archmotif loops, control plane) is
the agent's, not the embedder's.

Four operations cover the common cases:

1. **Emit** an event for fire-and-forget delivery.
2. **Invoke** a YAML-declared command and await its result.
3. **Read** the projection for a request (snapshot or by key).
4. **Subscribe** to a stream of events for ongoing consumption.

Plus a fifth, optional:

5. **Describe** the handler graph (for foreign tooling that wants to
   introspect).

## A. Go embed package (`pkg/embed/`)

In-process, same Go binary, no socket, no daemon. Ideal for tools and
embedded use. The surface:

```go
package embed

type Config struct {
    ConfigPath  string                   // YAML file path
    Source      string                   // default Source attribution
    MaxSteps    int                      // optional cap
    Trace       bool                     // append every event to Result.Events
    HandlerExts []handler.Factory        // additional handler types to register
}

type Args map[string]any
type Result struct {
    RequestID  string
    Events     []event.Event
    Projection map[string]any
}

func New(cfg Config) (*Agent, error)

func (a *Agent) Emit(ctx context.Context, eventType string, args Args) error
func (a *Agent) Invoke(ctx context.Context, command string, args Args) (*Result, error)
func (a *Agent) Send(ctx context.Context, text string) (*Result, error)
func (a *Agent) Subscribe(ctx context.Context, opts SubscribeOpts) (*Stream, error)
func (a *Agent) Projection(requestID string) map[string]any
func (a *Agent) Describe() Graph

func (a *Agent) Close() error
```

Usage:

```go
agent, err := embed.New(embed.Config{ConfigPath: "calc.yaml"})
if err != nil { return err }
defer agent.Close()

// fire-and-forget
_ = agent.Emit(ctx, "MessageReceived", embed.Args{"text": "hi"})

// invoke-and-await: result is whatever the YAML-declared wait
// predicate resolves on (typically a projection slice)
result, err := agent.Invoke(ctx, "classify", embed.Args{"item": "foo"})
if err != nil { return err }
verdict := result.Projection["calc.verdict"]

// stream consumer
stream, _ := agent.Subscribe(ctx, embed.SubscribeOpts{EventType: "ResponseEmitted"})
for ev := range stream.Events() { /* ... */ }
```

### Semantics

- `Emit` returns once the event has been published. It does not wait
  for drain. Suitable for fire-and-forget patterns where the caller
  doesn't need a result.

- `Invoke` looks up the command's YAML binding (`events:[].cli.command`
  matches `invoke <command>`) and emits the bound event type with
  `args` mapped to the declared payload keys. It then waits on the
  binding's `wait` predicate. The default wait is `drain` when none is
  declared. The `Result` carries the request_id, the full event trace
  for that request, and the projection snapshot.

- `Send` is sugar for `Invoke` with the command `send` and `args:
  {text: ...}` — equivalent to the CLI's `reflex send <text>` form.

- `Subscribe` opens a read-side stream of events matching
  `SubscribeOpts.EventType` (or the full stream when empty). The stream
  delivers events as they are appended. Closing the agent or the
  stream's context cancels delivery.

- `Projection` returns the projection snapshot for a request_id.
  Read-only; callers must not mutate.

- `Describe` returns the live handler graph (Phase 4b: the live table;
  pre-4b: the static compiled graph) for foreign tooling that wants to
  understand the topology.

### Thread safety

`Agent` is safe for concurrent use. Multiple goroutines can `Emit` /
`Invoke` / `Subscribe` simultaneously. The bus serialises dispatch
internally; the embedder's calls queue at the publish boundary and
drain in order.

### Error handling

Errors surface as Go `error` returns. Handler failures (the bus
emitting `HandlerFailed`) propagate as an error from `Invoke` /
`Emit` if they prevent completion. The full failure trace is on the
event log; `result.Events` carries the `HandlerFailed` event for
post-mortem inspection.

## B. HTTP daemon API

The Phase 4a daemon exposes the same operations over HTTP. Use this
when the embedder is a different process or in a different language.

### Endpoints

```
POST /emit
  body: {event_type, args, wait?}
  200:  {result}    # when wait is satisfied
  202:  (empty)     # when wait is empty (fire-and-forget)

POST /invoke/:command
  body: {args, wait?}
  200:  {result}    # wait predicate from YAML cli binding by default
  202:  (empty)     # when caller explicitly disables wait

POST /send
  body: {text, wait?}
  200:  {result}    # sugar for MessageReceived
  202:  (empty)     # when caller explicitly disables wait

GET /projection/:request_id
  200:  {<key>: <value>, ...}

WS /subscribe?event_type=<X>
  Streams events as JSON objects (one per WebSocket message).

GET /describe
  200:  {handlers: [...], edges: [...], events: [...]}
```

### Conventions

- **`result`** is a JSON object: `{request_id, projection: {...},
  events: [...]}`. The events array is only populated when the request
  used `?trace=true` (off by default to keep payloads small).
- **`wait`** is the same predicate vocabulary as the CLI: `drain` /
  `request_id_terminal` / `projection.has=<key>`. Empty string means
  fire-and-forget.
- **404** for a `request_id` that has no projection state and no log
  entries.
- **400** for unknown `command` / `event_type` / malformed `args`.
- **500** for handler failures or bus errors. Body carries
  `{error: <message>, request_id?: <uuid>}`.

### WebSocket subscriptions

`WS /subscribe?event_type=<X>` opens a server-pushed stream of events.
Each WebSocket message is one event as JSON. Empty `event_type` streams
everything. Closing the WebSocket cancels the subscription.

The daemon does not store unbounded buffers; backpressure is the
client's job. A slow client may see the daemon close the socket if its
send queue exceeds a configured high-water mark (default 1024 events).

### Wire shape

The wire shape mirrors `pkg/event/event.go` exactly:

```jsonc
{
  "id": "<uuid>",
  "type": "ClassificationResult",
  "request_id": "<uuid>",
  "ts": "2026-06-10T20:00:00Z",
  "source": "classify",
  "caused_by": "<event id>",
  "terminal": false,
  "payload": { "classification": "STUCK", "reason": "..." }
}
```

The HTTP API does not invent a separate envelope. The bus's events
are the contract.

## C. Optional gRPC

A gRPC service mirroring HTTP can be added when a use case demands it
(streaming-heavy clients, polyglot environments without HTTP+JSON
tooling). The proto follows the HTTP shape:

```protobuf
service Reflex {
  rpc Emit(EmitRequest) returns (EmitResponse);
  rpc Invoke(InvokeRequest) returns (InvokeResponse);
  rpc Send(SendRequest) returns (InvokeResponse);
  rpc GetProjection(ProjectionRequest) returns (ProjectionResponse);
  rpc Subscribe(SubscribeRequest) returns (stream Event);
  rpc Describe(DescribeRequest) returns (DescribeResponse);
}
```

Deferred unless needed. The HTTP + WebSocket combination covers the
common cases without an extra build dependency.

## Authentication

Authentication is **out of scope for v1**.

The Go embed package is in-process — the deploying app's trust boundary
is sufficient. The HTTP daemon is intended to run behind a
deployment-controlled trust boundary (loopback only, or behind a
reverse proxy that handles authn/authz). Adding mutual-TLS or token
auth at the daemon level is a deployment concern, not a reflex
concern.

The Phase 4c permission model applies to handlers inside the bus, not
to embedder callers. An embedder calling `Emit("AnyEvent", ...)` is
the *root principal* from the bus's perspective — it can emit any
event type into any scope. Embedders that need finer-grained control
over what an external caller can do should layer it at the deployment
boundary.

## Property: small surface

The full embedder API surface — Go + HTTP — fits in ~10 endpoints and
~6 functions. Foreign apps don't see:

- the handler graph (except via the optional `/describe`),
- the bus internals (`pkg/bus`, `pkg/event`),
- the compression machinery,
- the permission system,
- the projection store as a struct (only its snapshot shape),
- the analyzer.

This is the asymmetry reflex aims for: rich internal model (Phases 1–8)
behind a thin external API. An embedder integrates by reading nine
endpoints; the system behind those endpoints can be any topology the
bus can express.

## Property: same shape as the CLI

Every operation an embedder performs has a CLI equivalent:

| Embed call                      | CLI equivalent                                                     |
|---------------------------------|--------------------------------------------------------------------|
| `Emit(type, args)`              | `reflex emit --type <type> --payload <json>`                       |
| `Invoke(command, args)`         | `reflex invoke <command> <args>`                                   |
| `Send(text)`                    | `reflex send <text>`                                               |
| `Projection(request_id)`        | (CLI prints projection on `--trace`)                               |
| `Subscribe(opts)`               | (CLI streams to stdout on `--trace`)                               |
| `Describe()`                    | `reflex describe --config <yaml>`                                  |

The embedder is not a separate API; it is a programmatic version of the
existing CLI. The wait-predicate vocabulary is identical. The result
shape is identical. The `Args` map maps directly to the CLI's
positional arguments via the YAML `events:[].args` declaration.

This is a deliberate design choice: a developer who has learned the CLI
can use the embed API without relearning anything. A debugger inspecting
an embedder's behaviour can run the same configuration via the CLI and
see the same trace.

## Property: in-process or remote, same semantics

The Go embed package and the HTTP daemon expose the same semantics. A
developer can prototype with `pkg/embed` in a unit test, deploy with
the HTTP daemon in production, and the application code doesn't change
beyond replacing the agent constructor. The daemon is just a long-lived
embed bus with a network adapter.

This is what makes the "Phase 4a: bus daemon + remote handler SDK over
Unix socket" milestone tractable: the daemon's API is the embed API,
the SDK's handler protocol is the same `Subscriber` interface, and the
wire format is the JSON event envelope. Same primitives; different
transport.
