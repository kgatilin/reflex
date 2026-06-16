# Doc 32 — Architect end-to-end debug log

**Goal (pragmatic):** get the architect meta-agent to (1) compose a valid worker
topology, (2) build + dispatch it, (3) have the worker actually solve a task —
a non-empty, correct diff. Whatever it takes. This doc records every fix, hack,
and blocker so we can later decide the RIGHT design. It is a running log, not a
spec — entries are append-only, newest insight wins.

**Iteration loop:** run the architect LOCALLY with the brain on
`vertex:gemini-3.5-flash` (pro-preview is unreachable from local ADC; flash works
locally, proven by the worker run). Fast loop, no Docker. Once the plumbing is
green locally, validate on the real SWE-bench instance in the container with the
stronger pro model.

Legend: ✅ fixed · ⚠️ worked around (revisit) · ❌ open blocker · 💡 design note

---

## Confirmed-good (pre-existing, verified this session)

- ✅ **Worker tool-result rendering.** A worker whose `worker.history` sets
  `params.emits` to the tool `.call` kinds renders call→result as STRUCTURED
  function call/response. Proven end-to-end: worker read→edit→re-read→fixed a bug
  (`return a + b`). Debug log now shows `tool_call=`/`tool_result=` per message.

- ✅ **Cycle-aware dead-end guidance.** A rejection names where a dead-end kind can
  connect without a cycle, or says "register it terminal". (engine/validate.go)

- ✅ **Catalog query.** `topology.events.list` → engine answers
  `topology.events.catalog` with every registered kind + terminal + schema.

- ✅ **Structured call/result mapping for the control plane.** `historyParams`
  gains `calls` / `results` / `ack_calls` so the brain's NON-tool control calls
  (topology.subscriber.add, task.new) render as structured function calls (with
  thought signature) and outcomes pair back as responses. Without it they were
  plain text — likely a driver of the brain's idle/erratic turns.

---

## Blockers found while driving the architect (this push)

### B1 — ✅ Structured control-plane mapping works (turn 1 proves it)
Local run, brain on `gemini-3.5-flash`. Turn 1: `calls=4` (composed 4 pieces in one
turn), and the history grew to `messages=9` = 1 task + 4 assistant function-calls +
4 synthetic acks. So the `calls`/`results`/`ack_calls` mapping renders the control
plane as structured function calls/responses as designed. Not the blocker.

### B2 — ✅ Provider had NO retry; a rate limit killed every turn
Turns 2–30 produced no response. Root cause (from the `brain.failed` payloads, not
slog): `Error 429 … RESOURCE_EXHAUSTED`. The architect drives the model in a tight
loop and trips the per-minute Vertex quota; each 429 became a `node.failed` and the
run burned its whole budget on throttling — looked like the brain "going idle", but
it was transport throttling.
- Fix: exponential backoff retry (2s→30s cap, 6 attempts) on 429 / 5xx in the
  Gemini adapter (`pkg/provider/gemini_vertex.go`). Non-retriable errors break out.
- 💡 Design note: retry belongs in the transport, but the architect's tight drive
  loop is the real pressure. Worth revisiting: pace the brain, or a shared rate
  limiter, so a fleet of agents doesn't collectively trip the quota.
- ⚠️ Open question: if the 429 is a project/daily quota (not per-minute), backoff
  won't save it locally — may need a higher-quota project or the container.

### B3 — ❌ Gemini thinking: "Function call is missing a thought_signature" (400)
With backoff past the 429, the REAL structural bug: a Gemini 3.x thinking model
requires every replayed `functionCall` in history to carry its `thought_signature`.
But for PARALLEL function calls (the brain emits `calls=4`…`21` in one turn) Gemini
puts the signature on only the FIRST call part. reflex stores each call as a
SEPARATE event and replays each as its OWN model content → calls 2…N have no
signature → 400 on the next turn. The whole run then fails every turn.

Compounding: my `ack_calls` insert a synthetic function RESPONSE after each call
(`call, ack, call, ack…`). But parallel calls are ONE model content followed by ONE
user content of responses — you cannot interleave a response between them. So the
ack design is also wrong for multi-call turns.

Candidate fixes (deciding):
- ⚠️ PRAGMATIC: disable thinking for the brain (thinking budget 0) → no signatures
  required → no 400. Fastest unblock; composing a topology may not need deep
  thinking. (Trying first.)
- 💡 CORRECT (revisit): preserve Gemini's turn structure on replay — group the
  calls from one model turn into a single content (signature on the first part),
  then their responses into one following content. Needs a per-turn id on the call
  events + adapter/builder grouping. This is the real design gap: reflex flattens a
  multi-call turn into independent events, losing the provider's turn grouping that
  thinking models depend on.

### B3 — ✅ FIXED by signature propagation (Gemini accepts a repeated signature)
Smallest fix: in the body's emit loop, capture the turn's thought signature (the
one call that carries it) and attach it to EVERY call event from that turn
(`sig := tc.Signature; if empty → turnSig`). On replay each parallel call now
carries a valid signature. Gemini ACCEPTS the repeated signature — no 400. So
thinking stays ON (no need to disable it), and multi-call turns work.
- ⚠️ This repeats one signature across separate contents, which is not how Gemini
  models the turn. It works (presence check passes), but the CORRECT design is
  still to group a turn's calls into one content. Repeating is the pragmatic win.
- The interleaved-ack concern (B3) did NOT bite — Gemini accepted call,ack,call,ack
  as sequential function calling. Fine for now.

---

## ✅ MILESTONE — first GREEN end-to-end run (local, flash brain)

Task: "In calc.py, add(a,b) returns a-b but must return a+b. Fix it."
Result: the architect COMPOSED a worker, BUILT it (2 changeset.applied, **0
rejected**), DISPATCHED it (request.received), and the worker ran 9 turns making
real tool calls (3 reads, 4 edits, a test attempt) and FIXED the file:

    -    return a - b
    +    return a + b

So the whole chain works: architect designs + builds + dispatches → worker solves
the task → correct non-empty diff. Zero brain failures across 30 turns.

The fixes that got us here (this push):
1. provider 429/5xx exponential backoff (B2)
2. structured call/result mapping for the control plane (committed earlier)
3. thought-signature propagation across a turn's parallel calls (B3)

### Remaining polish (not blockers — the task still completed)
- ⚠️ The architect did not STOP after a successful dispatch — it kept composing
  and hit `scope.architect.budget_exhausted` (2 changeset.applied, 2 request.terminal).
  Works, but wasteful; the architect should quiesce once the worker is launched.
- 💡 The worker used the model binding the architect chose (flash locally). The
  worker tried `tool.py.test.call` even though pytest is absent in the local
  sandbox — fine here (the edit was correct), but the real container has pytest.
- 💡 CORRECT design still owed (revisit): preserve Gemini turn grouping on replay
  instead of repeating one signature; clean architect quiescence after dispatch.

### Next: validate on the real SWE-bench instance in the container (pro brain).

---

## Container run on the REAL instance (psf/requests-3362, pro brain)

The fixes held on `gemini-3.1-pro-preview`: **0 brain failures** across 30 turns
(no 429, no thought_signature 400). The pipeline ran fully.

**1) Architect** — 30 turns, 23 control calls, composed 1 scope + 5 events + 8
subscribers + 1 projection, **2 applied / 0 rejected**, dispatched the worker.
~117k in / 17k thought tokens (~$0.24). Clean.
- ⚠️ Over-dispatched: `request.received=4` (built/launched repeatedly) — the
  "doesn't quiesce after dispatch" issue again. Wasteful, not fatal.

**2) Worker topology it built** — CORRECT and complete: a `detached` `worker`
scope (root request.received, per-tool budgets 100, llm.message 30); a
`worker.agent` llm wired to all 5 tools' results/failures, emitting the 5 `.call`
kinds + `worker.done`; a `worker.history` projection with `params.emits` set right
(so tool results render structured — the fragility did NOT bite with the pro
model); tool subscribers + terminator + exhausted. A textbook worker.

**3) Worker** — ran 46 turns, **41 real tool calls** (24 search, 10 read, 2 edit,
7 test), ~461k in / 61k thought tokens (~$0.89). It worked hard.
- ❌ **NOT SOLVED.** The diff touches ONLY test files (`tests/test_requests.py`,
  `tests/test_utils.py`) — it added a test reproducing the bug and fixed an
  unrelated pytest idiom, but NEVER edited the source (`requests/models.py` /
  `utils.py` where `stream_decode_response_unicode` mishandles `encoding=None`).
  So the hidden fail-to-pass tests will still fail.

### Why the worker failed (root causes, for later)
- 💡 The ARCHITECT wrote the worker's system prompt as "…**write tests**, search…
  and edit the files to fix it." The worker over-indexed on the "write tests"
  instruction and never pivoted to the source fix. The meta-prompt should bias the
  worker toward fixing SOURCE, not authoring tests (SWE-bench supplies the tests).
- 💡 `max_tokens: 4000` on the worker is low for reading real source files —
  likely truncates context/edits.
- 💡 No feedback loop forcing "did the target test pass?" before `worker.done`;
  the worker declared done having only edited tests.

### Verdict
Plumbing: ✅ solved (architect designs+builds+dispatches; worker runs with
structured tool I/O; no transport/encoding failures). Task outcome: ❌ the worker's
REASONING/PROMPTING is the remaining gap — it builds and acts, but the
architect-authored worker prompt steered it wrong. Next lever is meta-prompt
quality (worker instructions), not kernel plumbing.
