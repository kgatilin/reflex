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
go run ./cmd/reflex --config examples/calc.yaml --message "what is 2+2"
# assistant: The answer is 4
```

Add `--trace` to dump the full event log as one JSON object per line:

```
go run ./cmd/reflex --config examples/calc.yaml --message "what is 2+2" --trace
```

Try the deliberately-broken stall example to see the watcher fire:

```
go run ./cmd/reflex --config examples/stall.yaml --message "anything" --trace
# assistant (stall): I will speak, but I will never close the request.
# ...trace lines including a RequestUnhandled event...
# request <uuid> unhandled: drain quiesced without RequestHandled
# exit status 2
```

## Repo layout

```
cmd/reflex/         single CLI entry point
pkg/event/          Event type + in-memory append-only store
pkg/bus/            dispatcher (drain function, not goroutine pool) + Subscriber interface
pkg/projection/     SessionProjection — pure fold of events for one request
pkg/handler/        built-in handler factories: llm_stub, tool_call, printer, terminator, unhandled_watcher, echo
pkg/config/         YAML loader + validation
internal/runtime/   glue: build a bus from a config, run a single user message
examples/           calc.yaml (working) + stall.yaml (intentionally broken)
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
```

### Handler types

| `type:`              | Subscribes to       | Emits                                      | `config:` fields |
|----------------------|---------------------|--------------------------------------------|------------------|
| `llm_stub`           | configurable        | `ToolCallProposed`, `AssistantMessageProposed`, `RequestHandled` | `rules[]`, `fallback`, `trigger_on[]` |
| `tool_call`          | `ToolCallProposed`  | `ToolResultObserved`                       | `tool` (one of `calc`, `echo`, `length`, `upper`) |
| `printer`            | typically `AssistantMessageProposed` | nothing                  | `prefix`, `field` (default `text`) |
| `terminator`         | configurable        | `RequestHandled` (if not already)          | (none) |
| `unhandled_watcher`  | `__noop__`          | `RequestUnhandled` (post-drain)            | (none) |
| `echo`               | configurable        | event type from `emit:`                    | `emit` |

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

## Tests

```
go test ./...
```

Covers the projection fold, dispatcher fan-out and bounded termination,
YAML parse + validate (including unknown-type rejection), the calc tool,
the printer, the watcher, the end-to-end calc scenario, and the stall
scenario producing `RequestUnhandled`.
