# 12 — ReAct from atomic nodes: experiment findings

A ReAct agent assembled from atomic nodes (`examples/react.yaml`), per the
domain model in [11-domain-model.md](./11-domain-model.md): the LLM node is
a pure fold→complete→emit translation with zero agent logic; routing,
decoding, tools, pumps are separate single-purpose reactions; the loop is a
declared graph cycle capped on the `brain` node. Backend: Vertex AI
`gemini-3.5-flash` via `google.golang.org/genai` (ADC).

Topology:

```
RequestReceived → seed → llm.turn → brain → llm.completed → decode
                            ↑                                  │
            pump-result/pump-failed                   tool.calc.call │ assistant.message(T)
                            ↑                                  │            + RequestHandled(T)
        tool.calc.result / tool.calc.failed  ←  calc  ─────────┘
```

All live runs below are real `gemini-3.5-flash` traces.

## What worked

### W1. Multi-step ReAct with no loop anywhere

"what is 23*7+11?" → `calc(23*7)=161` → `calc(161+11)=172` → final
"The result of 23*7+11 is 172." Three model turns, `RequestHandled`,
clean quiescence. No orchestrator, no scratchpad variable, no step
counter in any node — the loop is the topology, the scratchpad is the
log fold, the counter is the dispatcher's cap.

### W2. Self-correction is free topology

"compute 2 to the power of 10" (no power operator in calc):

```
brain: calc("2**10")  → tool.calc.failed: cannot parse "2**10"
brain: calc("2^10")   → tool.calc.failed: cannot find expression
brain: final "2 to the power of 10 is 1024."
```

The model saw its own failures in the transcript (non-terminal
`tool.calc.failed` → pump-failed → next turn) and adapted twice. There
is **no error-handling code anywhere in the system** — recovery is a
subscription. This validates the errors-are-events decision.

### W3. Graceful decline without hallucinated tools

"what is the weather in Paris?" → one turn, final "I do not have access
to real-time weather information...". No `tool.weather.call` was
invented (the type-level-gap/orphan path is covered by unit tests, not
exercised live).

### W4. Every answer is auditable by construction

See F2: whether an answer was tool-verified is a *projection over the
cone* — no successful `tool.calc.result` before the final message means
the claim came from model memory. No other agent architecture gives
this for free.

## Where the model fails

### F1. The loop cap is a guillotine

"5+6+7+8+9+10+11+12" with cap 6: six perfect steps (11, 18, 26, 35,
45, 56), then `LoopExhausted` **one addition short of the answer**. Six
successful tool calls are discarded; the user gets nothing. Worse, the
request then quiesces without `RequestUnhandled` — from the CLI it
looks closed, just silently answerless.

The deterministic enforcer cannot ask the model to wrap up; the model
cannot see the budget. Fix, already implied by the domain model: budget
is a *projection* (count `llm.completed` in the cone), and the
dispatcher (or a watcher reaction) emits a `scope.budget_low` event
into the cone one turn before the cap — the transcript fold then shows
it to the model, which can choose to answer with what it has. The
guillotine stays as the hard backstop.

### F2. Silent fallback to parametric memory

In the 2^10 run the final answer (1024, correct) was produced **after
two tool failures, verified by nothing**. The model fell back to its
own knowledge and the topology happily routed it as a normal final.
The log makes this *detectable* (W4) but nothing *acts* on it: a
trust-guard reaction subscribed to `assistant.message` that checks the
cone for verification could downgrade/annotate unverified claims. The
failure is that no stock node does this; the win is that it needs no
new mechanism.

### F3. Impurity bites only in the executed-but-unrecorded window

(Corrected after review — the original framing overstated this.)

Projection replay never re-executes anything: it folds *recorded*
events, so `llm.completed` is read as a fact and the LLM's
non-determinism is irrelevant to state reconstruction. Likewise there
is no stored "program counter": the execution position is itself a
projection — **the frontier**: non-terminal events with no
descendants. Resume after a crash = recompute the frontier from the
log and re-dispatch it. (The dispatcher's in-memory queue is a cache
of this frontier; today it is lost on crash and not yet rebuilt.)

The real exposure is the window between *executing* a reaction and
*appending* its events. Crash there → the frontier still shows the
trigger unreacted → re-dispatch → re-execution. At-least-once, the
classic event-sourcing problem. Per node class:

- **LLM node**: a duplicate Vertex call. The lost completion was never
  recorded, so the log never disagrees with reality — the cost is
  money and trajectory non-determinism, not correctness. Ironically
  the *safest* node: its only effect is the emitted event.
- **Side-effecting tools** (`tool.email.send`): re-execution means two
  emails. These need an idempotency key — the trigger event's `id` is
  the natural one; a tool that has already seen the id re-serves its
  recorded result instead of repeating the effect.

Cassette-style record/replay (`llm.completed` re-served from a
recording) remains legitimate, but as a *testing tool*, not a
correctness requirement: regression runs of a topology against a
recorded trace, and the Phase 6 optimiser's behavioural-equivalence
check over a trace corpus — re-running a rewritten graph against live
LLM sampling would compare noise with noise.

### F4. Signal events smuggle payloads

`echo` re-emits its trigger payload, so `llm.turn` arrives carrying
`{result: "172"}` or the user message. Harmless here — brain reads the
log, not the trigger — but it blurs the contract: a turn signal should
be empty. Pumps want a `signal` handler (emit empty payload), or echo
needs a `strip: true`.

### F5. Flat transcript will break on fan-out

`renderTranscript` folds the whole request linearly. Correct for this
single-branch pipeline; wrong the moment a turn fans out into parallel
branches — each branch's LLM call would see sibling noise. The fix is
already designed (11-domain-model.md): fold the *causal cone* of the
trigger, not the request; merge cones only at declared join nodes.
`caused_by` must become a list first.

### F6. Economics argue for crystallization

~2–3 s and one billed call per turn; eight turns ≈ 20 s for arithmetic
a deterministic node does in nanoseconds. The trace corpus shows the
brain's behaviour at this gap is regular (always one binary op at a
time). That regularity is exactly what Phase 6 optimisation-as-rewrite
should compile into a deterministic handler, demoting the LLM at this
gap from router to fallback.

## Verdict

The architecture held. Nothing in the failure list indicts the
three-concept model — every fix lands on an existing primitive
(budget = projection + event, trust = reaction, replay = log-as-truth,
cone-scoped transcript = projection). The failures are missing
*conventions and stock nodes*, not missing concepts.
