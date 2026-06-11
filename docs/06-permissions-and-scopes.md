# 06 — Permissions and scopes

Phase 4c. Shipped. Every handler declares a scope. The bus runs a
synchronous permission check before recording any handler-issued
control-plane mutation (`HandlerRegistered`, `Subscribed`,
`Unsubscribed`, `HandlerDeregistered`, `PermissionGranted`,
`PermissionRevoked`). On refusal the bus emits `PermissionDenied` and
leaves the live table unchanged. Boot-time YAML loading is the
bootstrap stream and is exempt from the check — the loader IS the root
authority.

This page is the contract. Cross-reference
[`05-control-plane-as-events.md`](./05-control-plane-as-events.md) for
the control-plane primitives the layer composes on top of.

## YAML grammar

Two new pieces of syntax: a per-handler `scope:` + `permissions:` and a
top-level `permissions:` block.

```yaml
permissions:
  # Top-level grants. Each entry produces one PermissionGranted event
  # at boot, BEFORE any HandlerRegistered fires.
  - principal: analytics-tool
    grants:
      mutate:     [tools.*, chat.*]
      read:       ["*"]
      forbidden:  [core.*, system.*]
      meta.grant: [analytics.*]

handlers:
  - name: fs-tool
    type: llm_stub
    on: RequestReceived
    scope: tools.fs.read             # the scope this handler "owns"
    permissions:                     # sugar — equivalent to a separate
      mutate: [tools.*]              # top-level entry naming this handler
      read:   ["*"]
    emits: [Done]
```

Field semantics:

- `scope:` — the dotted scope the handler owns. Used as the target of
  `HandlerRegistered`/`Subscribed`/`Unsubscribed` events when another
  principal mutates this handler. Defaults to `default.<name>` when
  omitted.
- `mutate:` — list of scope patterns the principal may publish
  control-plane events targeting.
- `read:` — list of scope patterns the principal may receive events
  from. (Phase 4c does not enforce read filtering at dispatch; the field
  is captured in the table for forward compatibility with the Phase 6
  filter primitive.)
- `forbidden:` — explicit deny list. Wins over `mutate` and `read`.
- `meta.grant:` — list of scope patterns the principal may delegate
  further (i.e. publish `PermissionGranted` events for). Default: empty.

Inline `permissions:` under a handler is syntactic sugar for a
top-level entry whose `principal:` equals the handler's name. The
loader applies both forms identically; tests assert equivalence (see
`TestInlinePermissionsEquivalentToTopLevel`).

## Scope matching

Scopes are dotted strings (`tools.fs.read`, `system.bus`,
`analytics.archmotif`). Patterns are exact strings or end with `.*`
(a "prefix match" wildcard). Special case: bare `*` matches any
non-empty scope.

Matching is **conservative**: `tools.*` matches `tools.fs.read`
and `tools.fs.read.line` but NOT bare `tools`. `*` matches any
target with one or more components — never the empty string and
never "zero components". Exact patterns (no trailing `.*`) require
full equality, not prefix promotion.

```
matchScope("tools.*", "tools.fs.read")           = true
matchScope("tools.*", "tools.fs.read.line")      = true
matchScope("tools.*", "tools")                   = false   (conservative)
matchScope("tools", "tools")                     = true    (exact)
matchScope("tools", "tools.fs.read")             = false   (exact)
matchScope("*", "tools")                         = true
matchScope("*", "")                              = false
```

This semantics was chosen for two reasons. First, the conservative
wildcard makes it impossible to silently grant authority over a parent
zone by writing the child pattern (`tools.fs.read.*` does NOT
authorise `tools.fs.read` itself). Second, exact-match patterns let
deployment YAML pin a single handler scope without an inadvertent
prefix overshoot.

## Permission events

Three event types — all terminal, all in the `system.permissions.*`
class, all emitted by the bus with `source: "bus"`.

```
PermissionGranted{principal, mutate, read, forbidden, meta_grant, granter}
```

Issued at boot (granter `"boot"`) or by a principal P that holds a
`meta.grant` matching every pattern in the grant (granter is P's
name). Updates the runtime permission table.

```
PermissionRevoked{principal, mutate, read, forbidden, meta_grant, revoker}
```

Reverses a previous grant. Same authority check as
`PermissionGranted` (the principal performing the revoke must hold
`meta.grant` over every pattern being removed).

```
PermissionDenied{principal, op, target_scope, reason}
```

Emitted by the bus when a control-plane mutation is refused. `reason`
is one of:

- `"forbidden"` — the principal's forbidden list matched, or the
  target was a reserved zone (`core.*` / `system.*` / `feedback.*`)
  and the principal had no explicit mutate grant naming the zone.
- `"out_of_scope"` — the target did not match any of the principal's
  mutate patterns.
- `"no meta-grant authority"` — emitted only on
  `PublishPermissionGranted`/`PublishPermissionRevoked`; the principal
  lacks a `meta.grant` pattern covering the target.

All three permission types are excluded from `EventDispatched`
recursion (see `isMetaEventType` in `pkg/bus/bus.go`).

## Bus enforcement layer

Every handler-issued control-plane operation goes through one of four
principal-attributed APIs on `*bus.Bus`:

- `SubscribeAs(principal, handler, eventType, maxIterations)` — checks
  `principal` against the target handler's scope, then delegates to
  `SubscribeWithCheck`.
- `UnsubscribeAs(principal, handler, eventType)` — same check, then
  delegates to `Unsubscribe`.
- `HandlerDeregisterAs(principal, handler)` — same check, then
  delegates to `HandlerDeregister`.
- `PublishPermissionGranted(principal, targetPrincipal, spec)` and
  `PublishPermissionRevoked(...)` — recursive meta.grant check.

A failed check emits `PermissionDenied` with the appropriate reason,
returns a non-nil error, and leaves the live table / permission table
untouched.

Boot-time registration uses the older `Register` / `Unsubscribe` /
`HandlerDeregister` / `SubscribeWithCheck` paths directly; they
bypass the permission layer. The YAML loader is the only caller of
those — runtime handlers MUST go through the `*As` family.

## Default-protected zones

`core.*`, `system.*`, and `feedback.*` are reserved. The
`*PermissionTable` seeds them with a default-deny posture: a runtime
mutation targeting any of them is refused with `reason: "forbidden"`
unless the principal holds an **explicit** mutate pattern naming the
reserved prefix. A bare `mutate: [*]` is NOT enough — wildcards do not
authorise reserved zones, so a sloppy "give it everything" grant
cannot stumble into the framework's own machinery.

```yaml
permissions:
  - principal: archmotif-internal
    grants:
      mutate: ["*", "core.dispatcher"]   # only core.dispatcher is
                                         # reachable, not the rest of core.*
```

This rule produced the four-line `matchExplicitReserved` helper in
`pkg/bus/permissions.go`; it is the only place in the engine where the
matcher consults the reserved-zone list.

## Recursive meta.grant

`PermissionGranted` itself is a control-plane event. The bus checks
the granter's `meta.grant` set against every pattern across every axis
in the grant. If any pattern is outside the granter's authority, the
grant is rejected and `PermissionDenied{op: "meta.grant"}` fires.

```
P holds meta.grant: [tools.*]
P publishes PermissionGranted{principal: Q, mutate: [tools.x]}      OK
P publishes PermissionGranted{principal: Q, mutate: [analytics.*]}  DENIED
P publishes PermissionGranted{principal: Q, mutate: [tools.x, analytics.x]}
                                                                    DENIED  (any one out of scope = deny)
```

Boot grants do NOT pass through the check — the YAML loader is treated
as implicitly holding `meta.grant: [*]`. This is what makes the grant
tree rooted: every non-root grant derives from a chain of `meta.grant`
authority terminating in the boot stream.

## Default scope + implicit grant

A handler with no `scope:` and no `permissions:` block gets:

- scope `default.<name>` (so its identity in the table is unambiguous);
- an implicit `PermissionGranted{principal: <name>, mutate: [default.*], read: ["*"]}`
  at boot, so it can mutate within the open `default.*` namespace.

This keeps Phase 1–4b YAML files working with no edits. Existing
examples (`calc.yaml`, `react.yaml`, etc.) operate in `default.*` and
never touch reserved zones; their runtime semantics are unchanged.

## Worked example

`examples/scoped_compression.yaml` ships a four-handler graph:

- `audit` (scope `system.audit`, read-only) — captures every
  control-plane event to a JSONL file.
- `reply-target` (scope `tools.fs.read`) — domain handler.
- `analytics-stub` (scope `analytics.archmotif`, mutate `[tools.*]`)
  — succeeds when it `SubscribeAs("analytics-stub", "reply-target", …)`.
- `feedback-saboteur` (scope `analytics.saboteur`, no feedback grants)
  — fails with `PermissionDenied{reason: "forbidden"}` on any
  `feedback.*` target.

The `TestScopedCompressionExample` runtime test exercises both paths
and asserts the audit log captured both the grant stream and the
denial.

## Non-goals

The Phase 4c layer is **handler-scoped**. Per-request permission
checks ("user X can only mutate handlers tied to requests X started")
are explicitly out of scope. A `request_id`-aware permission axis can
be layered on top later without changing the grant-event vocabulary
— the dispatcher would consult a request-scoped table in addition to
the handler-scoped one. None of that lives in Phase 4c.

Also out of scope:

- Read-side filter delivery (Phase 6 compresses subscriptions through
  the same plane and gets the filter primitive then).
- Compression / optimisation passes that USE permissions (Phase 6).
- archmotif as a live subscriber (Phase 7).

## Wire format additions

Phase 4c adds one field to the `HandlerRegistered` event payload:

```json
{
  "name": "fs-tool",
  "consumes": "RequestReceived",
  "emits": [...],
  "description": "...",
  "scope": "tools.fs.read"
}
```

The SDK protocol (`pkg/sdk/protocol.go`) does NOT change in Phase 4c.
Scope + inline permissions land on the in-process bus via
`sdk.WithScope`/`sdk.WithPermissions` options on the handler builder;
the remote SDK path inherits the default-zone implicit grant and is
free to mutate `default.*` only — TODO(phase-4d) extends the remote
hello frame with scope/permission fields once a multi-host story
exists.
