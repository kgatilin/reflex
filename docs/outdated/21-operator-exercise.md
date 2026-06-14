# 21 — Operator exercise: a great coding agent through topology alone (DRAFT)

> **Status: DRAFT / findings.** A dogfooding stress-test of the converged
> model ([14](./14-target-coding-agent.md)–[20](./20-topology-management.md)):
> assume the engine exists and an operator must turn the minimal coding
> agent into a *great* one under a hard constraint — **the operator never
> edits code or acts directly; every task goes through the agent; the only
> lever is the agent's subscribers** (nodes, subscriptions, scopes,
> projections — i.e. changesets, doc 20). The exercise is doc 08's
> optimisation story performed by hand. Most of it passes; the failures
> are the most valuable output and are ranked at the end.

## The operator loop

One working cycle, nothing else available:

```
read folds of closed requests  →  find a recurring failure or pattern
                               →  express the fix as a changeset
                               →  watch the next folds; keep or roll back
```

Every improvement is an audited, reversible, A/B-able topology change
(doc 20: history, diff, rollback for free). The corollary that shapes the
operator's life: **you can only steer what you can see** — half the job is
queries over the log ("every trace where an edit was not followed by a
lint"), which is doc 04 grown into a trace workbench. That is tooling,
not model, but without it the loop is blind.

## The iteration ladder

Each step is one changeset; the brain's code and prompt are never the
first resort — the environment is.

1. **Minimum** — doc 14 as written: brain + fs/fmt/lint plugins +
   `transcript` + `fs.seen` + the request budget.
2. **Verification gate.** Folds show answers landing without a lint. The
   gate pattern of doc 18: brain emits `draft.message`; a deterministic
   gate folds the cone — a `lint.result{ok}` after the last edit lets it
   through as `assistant.message`, otherwise `verify.needed` sends the
   brain back. Plus a `test.run` plugin. The brain was not edited;
   "answering unverified" was made inexpressible.
3. **Explore subagent.** The brain burns context on search. Register a
   second `llm` node on `tool.agent.explore.call` — and the model pays
   out its most elegant dividend: **subagent context isolation is a
   horizon.** The subagent's transcript view bound `in:` its own scope
   sees only its cone; the parent folds one result event. Fresh-context
   delegation — a dedicated mechanism in conventional agent runtimes —
   is one `in:` line. The subagent appears in the brain's menu
   automatically (the `tool.*.call` consumer projection).
4. **Repair lanes** — args-repair and a medic on `tool.*.failed`
   (doc 18), once the failure statistics show which errors are
   systematic rather than informative.
5. **Reviewer judge** — a second model subscribed to `scope.brain.closed`
   (doc 16: handoff is one `on:` line), scoring each step or the final
   draft.
6. **Memory and project context.** The world enters the log only through
   reactions (projections fold the log, never the disk — by design): a
   context-loader on `request.received` emits `state.updated.context.*`;
   memory-extract on `assistant.message` (doc 18) accumulates episodic
   facts the brain `reads:`.
7. **Crystallisation.** The corpus shows edit is always followed by
   fmt+lint → a changeset compiles the deterministic chain (docs 08/18);
   request budgets tighten to observed usage.

## What the model carries (the wins)

- **Improvement without touching the agent**: gates, lanes, judges,
  chains, budgets — all changesets; all audited, reversible, A/B-able.
- **Subagent isolation = horizon** (above) — zero mechanism.
- **The menu is live**: adding/removing a tool plugin re-teaches the
  model on the next firing, no config.
- **Verification is enforceable topology** (the gate), not prompt
  discipline; read-before-edit (doc 19) already set the precedent.
- **Cost control is scoped**: per-request and per-subagent budgets with
  `budget_low` warnings — the operator tunes spend per region of work.

## The gaps, ranked

### 1. Context budget and view compaction — the largest hole

Engine budgets count **events**; a coding agent dies of **tokens**. A
`log`-shaped transcript with `in: session` is an unboundedly growing fold
shipped whole into every firing. The `log` shape has no window (`last: N`)
and no compaction story. What is missing:

- a window in the view declaration;
- a **compactor pattern**: a reaction summarises (e.g. on
  `scope.request.closed`) into `state.updated.summary.*`;
- the unresolved core: a **compaction event as a horizon cut** — after a
  checkpoint, the view folds summary + tail rather than everything since
  the session root. Today nothing in the declaration grammar can say
  "fold from the last checkpoint".

Without this, a "great" agent hits the token wall within a few requests.
This is the next design document.

### 2. Payload weight in the log

`tool.fs.read.result` carries file content; the log is append-only and
forever. Forty files of a thousand lines and the log is a blob store.
Either accept it explicitly (event sourcing all the way) or define a
sidecar blob store keyed by sha with pointer events — in which case G1
("everything recomputes from the log") must declare the blob store part
of the log. Undecided; must be decided, not drifted into.

### 3. Mid-flight steering — machinery exists, the rule is unwritten

The user types "stop — don't touch the tests" while the request cone is
open. Causal positioning (doc 19) bites: the steer ingress is causally
incomparable with the open cone, so the brain's next firing **cannot see
it** — it is not in the trigger's causal past. The resolution needs no
new machinery, only a written rule: steering = `sys.scope.{instance}.cancelled`
(doc 20 intervention) + the resolver chaining the new request through the
cancelled request's closure (doc 19) — so all partial progress, including
results whose dispatch was refused (G9: appended, not dispatched), is in
the new request's fold. You cannot change a mind mid-thought; you cancel
the thought and re-think with everything kept. One section, somewhere —
likely doc 19's session-chain rule gains the cancelled case.

### 4. Nodes have no `.updated` — and the prompt is node config

Under the constraint, the brain's prompt is part of a subscriber — the
operator's only way to "train" it. Today a config change is
deregister + register. Prompt engineering through changesets deserves a
first-class `sys.node.updated`: the log then versions prompts for free,
with "which change moved the metric" attached.

### 5. Streaming — declared a non-gap

`assistant.message` is atomic; tokens do not flow through the log. The
stance: streaming is a transport concern of the reply sink (which may
stream from the LLM provider directly and emit the final event); the log
records outcomes, not keystrokes. Stated here so it reads as a decision,
not an omission.

### 6. The operator's eval loop

Improving via changesets needs "replay this request against topology v2
and diff the folds". Replay (G5) and cassette records (12 F3 —
`llm.completed` as terminal record) nearly suffice; missing is the
operation **fork a log at a position under a different topology**.
Tooling over the model, not model — but without it the improvement loop
has no regression harness.

## Verdict

The exercise mostly validates the model: management, subagent isolation,
gates, and crystallisation are expressible with zero new primitives —
the properties the architecture was built for actually compose. The real
holes are #1 and #2, and they are the same hole at two altitudes: **the
fold must fit** — in the model's window and in a log of sane weight.
#3 is one written rule; the rest is tooling and small kinds. Context
windowing/compaction (#1) is the next design target.
