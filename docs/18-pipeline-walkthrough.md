# 18 — Walkthrough: a conventional assistant pipeline on scopes (DRAFT)

> **Status: DRAFT.** A modeling exercise: take the most ordinary
> assistant shape — ingress → intent parsing → state-driven enrichment →
> reasoning loop → tools/answer → post-answer hooks — and express it with
> the two primitives of [15](./15-primitive-reduction.md) and the scope
> machinery of [16](./16-engine-architecture.md), adding **no new
> primitives**. The exercise passes, and surfaces two *naming* decisions
> (subjects, not primitives) recorded at the end.

## The case

1. A user event arrives from a surface.
2. An LLM call parses intent into a structured verdict; the verdict is a
   state write.
3. Subscribers on that state react: check things, load context, add memory
   facts. Only when *all* of that settles may the main reasoning start.
4. The main LLM loops: each turn ends in tool calls or a message.
5. A tool's outcome may itself be a state write besides its result.
6. The final message has its own subscribers (delivery, memory extraction,
   trust checks) — "the answer" is not "the end".

## Topology

```yaml
scopes:
  - name: request                       # declared: budget/deadline live here
    root: request.received
    budget: { llm.completed: 20 }
  - name: intent                        # the phase barrier
    root: state.updated.intent
    closed_when: quiescent

nodes:
  - name: intent          # llm, structured output
    on:   request.received
    emit: [state.updated.intent]

  - name: memory-check    # tool (in-process)
    on:   state.updated.intent
    emit: [state.updated.memory.note]

  - name: profile-load    # tool
    on:   state.updated.intent
    emit: [state.updated.profile]

  - name: brain           # llm
    on:   [scope.intent.closed]         # + own-turn closures, implicit llm semantics
    emit: [tool.*.call, assistant.message, request.handled]

  - name: search          # tool
    on:   tool.search.call

  - name: reply           # tool, fire-and-forget sink
    on:   assistant.message

  - name: memory-extract  # llm — the post-final-message hook
    on:   assistant.message
    emit: [state.updated.memory.episodic]
```

## Trace

```
app.ingress.tg.message
 └─ session-resolver
    └─ request.received ─────────────────────────── scope «request» opens
       └─ intent (llm)
          └─ state.updated.intent{...} (T) ───────── scope «intent» opens
             ├─ memory-check → state.updated.memory.note (T)
             ├─ profile-load → state.updated.profile (T)
             └─ (intent-cone obligations → 0)
          scope.intent.closed{caused_by:[note, profile]}   ← engine
             └─ brain, turn 1: fold = text + intent + memory + profile
                ├─ tool.search.call ┐
                └─ tool.fetch.call  ┘──────────────── auto sub-scope T₁ (fan-out)
                   ├─ search → tool.search.result
                   └─ fetch  → tool.fetch.result + state.updated.cache.x (T)
                scope.closed{T₁}                      ← engine
                   └─ brain, turn 2: decides to answer
                      ├─ assistant.message (T)
                      │  ├─ reply → user sees the answer NOW
                      │  ├─ memory-extract → state.updated.memory.episodic (T)
                      │  └─ trust-guard → state.updated.trust.verdict (T)
                      └─ request.handled (T)
       (request-cone obligations → 0)
    scope.request.closed   → end of the OTel trace; audit fold
```

## What carries each requirement

**Phase sequencing — the named-scope barrier.** `brain` does not subscribe
to `state.updated.intent` (it would fire before the enrichers); it
subscribes to `scope.intent.closed`. Degenerate cases need no paths: zero
enrichers ⇒ zero obligations ⇒ instant closure; an enricher that itself
calls an LLM and fans out ⇒ a nested scope, and transitive quiescence
(doc 16) holds the barrier. What the barrier does *not* give is ordering
*within* the phase — enrichers run in parallel; "memory-check must see
profile" is a subscription chain, not a scope.

**"A message is just a state append" — inverted, in the user model's
favour.** Emitting an event *is* the state write, because state is a fold
of the log. Nobody writes the transcript into state: it is computed from
`assistant.message` / `tool.*.result` on demand. "Hang a handler on a
state append" is `on: assistant.message`. The explicit `state.updated.*`
kind exists only for KV-shaped facts (intent, memory, profile) — where a
downstream consumer needs the *key*, not the history. Two flavours of
state, one mechanism.

**Post-final-message work.** `reply` delivers at *dispatch* of
`assistant.message`; `memory-extract` and `trust-guard` keep working
inside the still-open request cone. "Final message" ≠ "request closed":
the user is answered immediately, the bookkeeping completes after, and
`scope.request.closed` records the true end (and the end of the request's
OTel trace). No one waits for extraction; nothing is lost.

**Tool outcome as state.** A tool's emit set is free to include
`state.updated.{path}` next to its result: `fetch` emits both
`tool.fetch.result` (for the turn fold) and `state.updated.cache.x` (a
recorded fact for later requests).

**Deterministic tool chains.** edit → fmt → lint is a tool whose `on:` is
the upstream's result kind. The `tool.{name}.call` convention exists to
make a tool visible in the LLM's projected menu; a reaction may subscribe
to any kind. Chains are topology, not a primitive.

**Intent failure.** `llm.failed` is non-terminal inside the intent cone;
the scope still quiesces, `brain`'s fold shows "no intent" and it works
from raw text — or a fallback subscriber on `llm.failed` emits
`state.updated.intent{unknown}`. Errors-as-events, as designed (doc 11,
12 W2).

## Variation: multi-model tool loop — repair lane and error lane

The loop under load: the smart model reasons but aims tools poorly; a
cheap model should repair tool arguments before execution; failures
should be handled separately (also by a cheap model); turns iterate.

```yaml
  - name: brain            # opus: reasons, aims poorly
    on:   [scope.intent.closed]          # + own-turn closures
    emit: [tool.*.draft, assistant.message, request.handled]

  - name: args-repair      # haiku: fixes arguments
    on:   tool.*.draft
    emit: [tool.*.call]                  # same name, repaired payload

  - name: tool-medic       # haiku: separate error lane
    on:   tool.*.failed
    emit: [tool.*.call]                  # repaired retry — a cycle, capped by budget
```

One turn, all three mechanics at once:

```
brain turn 1 (opus)
  ├─ tool.search.draft{args≈} ┐          turn sub-scope T₁
  └─ tool.fetch.draft{args≈}  ┘
     ├─ args-repair (haiku) → tool.search.call{args✓}
     │    └─ search → tool.search.result
     └─ args-repair (haiku) → tool.fetch.call{args✓}
          └─ fetch → tool.fetch.failed{timeout}
               └─ tool-medic (haiku) → tool.fetch.call{retry}
                    └─ fetch → tool.fetch.result
  (T₁ obligations → 0 — through draft→repair→call→fail→medic→retry→result)
scope.closed{T₁}
  └─ brain turn 2 (opus): sees the whole story — once
```

What carries it:

- **The brain reacts to its turn's closure, not to individual results.**
  Exactly-once per fan-out, full story in the fold. This is config, not
  dogma: a node that wants streaming reaction subscribes
  `on: [tool.*.result]` and fires per result — at the cost of N model
  turns per fan-out. Default is the barrier; direct subscription is a
  deliberate per-node choice.
- **The error lane stretches the barrier automatically.** `tool.*.failed`
  is non-terminal *inside the turn cone*; the medic's retry is a causal
  descendant, so the turn scope cannot close until repair settles — zero
  config. The medic→call→failed→medic cycle is capped by the request
  budget (`tool.fetch.call: 3`). The non-repair variant: the medic emits
  `state.updated.diagnosis.*` and the brain decides (12 W2).
- **The repair lane is not `relay` resurrected** (doc 15 dropped pure
  renames): args-repair transforms the payload through a model — an
  ordinary `llm` reaction with its own consume/emit.
- **The causal barrier is insensitive to chain length.** A counting
  aggregator ("N calls ⇒ await N results", the old Phase 1.6) breaks the
  moment a repairer or medic is inserted between call and result.
  Quiescence is a property of the graph, not arithmetic — interceptor
  lanes can be added and removed (including by the doc-08 optimiser as a
  rewrite) without touching any synchronization.
- **Iterations** are the chain of turn scopes T₁→T₂→…, bounded by
  `budget: {llm.completed: 20}` with `scope.budget_low` one turn ahead of
  the guillotine.

One wrinkle, config not primitive: the brain's tool menu is still
projected from the consumers of `tool.*.call` (the real tools' schemas),
while its emit kinds are draft-prefixed — one config line maps menu
actions to the draft suffix. *(The turn-budget question this section
originally left open is resolved: turn scopes are named after their node
— `scope.brain.turn.closed` — so a per-turn budget attaches at the node;
see doc 16, rooting.)*

## A clarification this exercise relies on

`state.updated.intent` is **terminal** (a recorded fact, doc 11) *and*
roots a phase scope with live descendants. No contradiction: terminality
governs **orphan accounting only** — a terminal leaf demands no
descendant. Obligation counting (doc 17) is independent of it: dispatching
a terminal event to N subscribers still opens N obligations, so the cone
stays open while they run. Terminal = "needs no reaction", not "gets no
reaction".

## Two naming decisions this surfaces (subjects, not primitives)

1. **State paths move into the subject:** `state.updated.intent`,
   `state.updated.memory.note` — not `state.updated{path: ...}`. With the
   path in the payload, "subscribe to this state key" needs a payload
   filter — exactly the payload-routing that doc 11 deprecates. With the
   path in the kind suffix, state subscription is a NATS wildcard
   (`on: state.updated.memory.>`), the same move tools already made
   (`tool.{name}.call`). The session registry becomes
   `sys.state.updated.session.binding.slack.{thread}` — literally a NATS
   KV bucket. The value stays in the payload.
2. **Named scopes get typed closure kinds:** a declared scope `name: intent`
   announces `scope.intent.closed`, so phase wiring is type-level — not
   `scope.closed{root: ...}` plus a payload filter. Turn scopes are named
   after their rooting node (`scope.brain.turn.closed`) — subscribable by
   the node itself (the loop) or by anyone else (handoff, judging); no
   scope is anonymous. See doc 16, rooting.

Both are refinements of the subject grammar in
[13-event-taxonomy.md](./13-event-taxonomy.md); neither adds a concept.

## Verdict

The pipeline needs: two reaction bodies, declared scopes with one
non-default nothing (all `closed_when` are the default `quiescent`),
auto-rooted turn scopes, obligation counting, and the subject grammar.
**Zero new primitives.** The pressure the case applies lands entirely on
naming — which is where it should land.
