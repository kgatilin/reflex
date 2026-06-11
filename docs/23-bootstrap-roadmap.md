# 23 — Bootstrap roadmap: the task queue + cost protocol (LIVE)

> **Status: LIVE / operational.** Doc [22](./22-bootstrap-self-hosting.md)
> is the plan; this is the working document the operator runs the bootstrap
> from. Stage 0 is hand-built and shipped (see "What shipped" below). From
> here, **the roadmap is executed through reflex**: every task in the queue
> is a prompt sent to the agent of `examples/agent.yaml`, and every task's
> cost is measured and logged. The operator writes no more code — only
> tasks, reviews, commits, and topology edits.

## What shipped (stage 0, hand-built — the last hand-written code)

| Piece | Where | Doc-22 item |
|---|---|---|
| Provider interface + Vertex-Anthropic adapter, structured tool-call decode | `pkg/provider` | kernel #1 |
| Multi-model `llm` body: native tool calls, `llm.usage` per completion | `pkg/handler/llm.go` | kernel #1 |
| `fs` plugin: read/edit/write/search, `--root` clamp, sha crutch guard | `plugins/fs` | kernel #2 |
| `go.build` / `go.test` / `go.vet` validators | `plugins/gotool` | kernel #3 |
| Operator surface: `reflex emit --daemon`, fold via trace, `reflex costs` | `cmd/reflex` | kernel #4 |
| Cost tracking: price table, JSONL fold, audit `types:` sink | `pkg/cost`, `pkg/handler/audit.go` | (this doc) |
| The stage-0 topology | `examples/agent.yaml` | the crutch |

Run instructions are in the header of `examples/agent.yaml`.

## Operating loop (per task)

1. **Start clean.** Daemon + both plugins up; `git status` clean;
   `rm -f /tmp/reflex-agent-usage.jsonl` so the cost report is per-task.
2. **Send the task** as one `RequestReceived` (stage 0: one task = one
   request; size tasks to fit the window — the per-doc delta lists below
   are already nearly task-sized).
3. **Review** the fold (trace) and `git diff`. The agent cannot commit;
   the verdicts (`tool.go.*.result{ok}`) in the fold are the acceptance
   evidence.
4. **Commit or reject.** Reject = revert the tree and re-send a sharper
   task; the failed trace is data (what was under-specified?).
5. **Measure**: `reflex costs --log /tmp/reflex-agent-usage.jsonl
   --by-request` and append a row to the cost log below.
6. **Restart to adopt**: rebuild + restart the daemon/plugins between
   tasks (the self-hosting restart rule — only at quiescence, doc 22
   constraint #1). When a landed feature is usable by the agent's own
   topology, the next change is the operator migrating `agent.yaml` onto
   it — the migration is the acceptance test.

## The task queue (doc 22 self-build order)

Ordered by "what improves the builder fastest". Each iteration lists the
paste-ready first task; later tasks in an iteration are cut from the same
doc's delta list.

### Iteration 1 — calibration: the two remaining model adapters

Additive Go, obvious tests, immediately useful (cheap models for future
lanes) — and it calibrates the task size the agent can carry.

- **Task 1a — Gemini adapter.**
  > In pkg/provider, add a Gemini adapter behind the existing Provider
  > interface, registered under key "vertex" (binding strings like
  > vertex:gemini-2.5-flash). Use google.golang.org/genai (already a
  > dependency; see pkg/handler/llm_gemini.go for the client/auth pattern
  > to follow — ADC, cached client per project|location). Translate Tools
  > to native Gemini function declarations and decode function calls into
  > provider.ToolCall with dotted names; fill Usage from the response's
  > usage metadata. Mirror the structure and test style of
  > anthropic_vertex.go: pure request-build and response-decode helpers
  > with unit tests, no network in tests. Validate with go.build, go.test
  > ./pkg/provider, go.vet.
- **Task 1b — OpenAI-compatible adapter** (Model Garden: Llama, DeepSeek,
  Qwen) under keys like `vertex:deepseek`; same shape, Vertex's
  OpenAI-compatible endpoint, ADC.
- **Adopt:** nothing in the topology changes yet; the adapters are
  inventory for iterations 3+ (cheap lanes, R1 no-tools seats).

### Iteration 2 — obligation counting (17) + node-rooted scopes (16)

The engine work that kills the crutch loop. Tasks cut from docs 16/17
delta lists (per-scope obligation counter; `scope.brain.closed` emission;
scope-qualified subscriptions).

- **Adopt:** `agent.yaml` migrates — `seed` and all 15 pumps die, brain
  subscribes to `request.received` + `scope.brain.closed`, parallel tool
  calls become legal (delete the first-call-only crutch and
  `llm.calls_dropped` in `pkg/handler/llm.go`), the loop cap moves to a
  request-scope budget. The migration is the acceptance test.

### Iteration 3 — projections (19)

The walk evaluator, `reads:` / attach-at-dispatch, plugin-registered
projections.

- **Adopt:** the fs plugin's in-process sha map is deleted; the
  `fs.seen` kv projection carries the read-before-edit guard (doc 14).
  The transcript becomes a declared projection instead of
  `renderTranscript`'s hardcoded fold.

### Iteration 4 — changesets (20)

The changeset pipeline, reject/lint validation, scopes and projections as
managed objects.

- **Adopt:** from here the operator improves the running agent live, and
  doc 21's ladder starts in earnest: verification gate (ladder #2),
  explore subagent on `vertex:gemini-2.5-flash` (ladder #3), repair lanes
  (doc 18) — each a changeset, each measured by the cost log.

### Iteration 5 — onward

Budgets / deadlines / cancellation (16), context compaction (21 gap #1 —
unblocks multi-request tasks), G5 frontier rebuild (unblocks restart
during a task), the rest of docs 21/22. Re-prioritize against the cost
log and the failure traces accumulated by then.

## Cost tracking & optimisation

**Mechanism.** Every completion emits a terminal `llm.usage` (tokens incl.
cache, model binding, stop reason). The `usage-meter` handler in
`agent.yaml` sinks them to `/tmp/reflex-agent-usage.jsonl`;
`reflex costs --log <file> [--by-request] [--prices <json>]` prices them
(longest-prefix model table, `pkg/cost.DefaultTable`; override file when
prices drift).

**Protocol.** One usage file per task (step 1 of the operating loop), one
row per task in the log below. The point is a per-task $ number next to
the task's outcome, so optimisation decisions are made on data, not vibes.

**Levers, in the order to pull them:**

1. **Task sizing** — the dominant stage-0 cost driver. Every extra turn
   re-sends the whole transcript (the legacy fold has no caching); a task
   that takes 30 firings costs quadratically more than two tasks of 15.
   Watch `input_tokens` growth across a request in `--by-request`.
2. **Model–role fit (doc 22 table)** — after iteration 1/4, move
   non-brain seats to cheap models: explore subagent and repair lanes →
   `vertex:gemini-2.5-flash`; planner/judge (no `tool.*.call` in the emit
   set) → `vertex:deepseek/deepseek-r1`. One config line per seat; the
   cost log before/after is the justification. The brain stays strong
   (doc 22: stage-0 bottleneck is success rate, not cost).
3. **Prompt caching** — deliberately deferred (doc 22); the neutral log
   does not obstruct it. When the per-task input-token totals dominate,
   this is the engine task to schedule: a stable transcript prefix +
   cache_control in the anthropic adapter (cache read is 0.1× input).
4. **Brain downgrade experiments** — periodically re-run a standard task
   with a cheaper brain (one config line) and compare verdicts + cost;
   also the doc-22 "R1 stress-test brain" robustness check once repair
   lanes exist.

**Cost log.** (model = brain binding; $ from `reflex costs`; verdict =
accepted/rejected + commit)

| date | task | model | turns | in tok | out tok | $ | verdict |
|---|---|---|---|---|---|---|---|
| — | — | — | — | — | — | — | — |

## Open questions carried from doc 22

Unchanged: when (if ever) the agent gets `git.commit` (lean: after the
verification gate + confirm-gated flow); changeset rights (lean: never
directly); where task definitions live (prompts first — this file is the
operator's queue, not the agent's input).
