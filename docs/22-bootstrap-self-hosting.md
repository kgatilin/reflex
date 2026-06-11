# 22 — Bootstrap: reflex builds reflex (DRAFT)

> **Status: DRAFT / plan.** The self-hosting experiment: a coding agent
> running *on* reflex implements the rest of the roadmap *in* reflex. The
> operator writes a minimal hand-built kernel (stage 0) and from then on
> only sends tasks and edits topology
> ([20](./20-topology-management.md) / [21](./21-operator-exercise.md));
> the agent writes all code. Companion decisions: the multi-model `llm`
> body (any Vertex AI model per node), and the safety stance — **the
> agent gets no bash tool**, only the named deterministic plugins of
> [14-target-coding-agent.md](./14-target-coding-agent.md). Builds on
> [12](./12-react-experiment.md) (the shipped legacy loop, reused as the
> bootstrap crutch), [15](./15-primitive-reduction.md)–[21](./21-operator-exercise.md).

## Multi-model: a delta in the llm body, zero deltas in the model

Doc 18 already assumes per-node models (an opus brain, haiku repair
lanes); the model treats it as `config`. All work lands inside the `llm`
body:

- **One provider interface**: `(messages, tool schemas) → completion |
  tool calls`. A node binds a model by string:
  `vertex:gemini-2.5-pro`, `vertex:anthropic/claude-opus-4-8`,
  `vertex:meta/llama-4-…`.
- **Three adapters behind Vertex AI** (not literally one Google library):
  Gemini via `google.golang.org/genai`; Claude via `anthropic-sdk-go`
  with the Vertex backend; Model Garden OSS models (Llama, DeepSeek,
  Qwen, …) via Vertex's OpenAI-compatible endpoint. Authentication is
  one story for all three (ADC), so operationally it is a single
  integration.
- **Tool-call dialects are translated in the adapter; events stay
  canonical.** The log is provider-neutral, which combines with cassette
  records (12 F3) into free model A/B: replay one trace against another
  model and diff the folds. Prompt-cache optimisation is deliberately
  deferred; a neutral log does not obstruct it.

## The frame: stage 0 is the last hand-written code

This is a compiler-style bootstrap. The deliberate choice is **not** to
hand-implement the converged model (docs 16–20) first, but to assemble
stage 0 from what is already shipped: the 12-era loop (`seed` / pumps /
`llm.turn`), loop caps, the daemon, SDK Remote, control plane 4b/4c. The
legacy scaffolding is a knowing crutch — the agent later removes it with
its own hands, and each removal doubles as the acceptance test of the
feature that replaces it.

## Stage 0: the hand-built kernel

The axe before the forge — four pieces, nothing more:

1. **Provider interface + the Vertex-Anthropic adapter + structured
   tool-call decode** (doc 14 delta #1). The one place not to skimp:
   these are the agent's hands, and they must aim precisely.
2. **The `fs` plugin** (read / edit / write / search; `--root` clamped to
   the reflex repo; line-numbered read windows; unique-`old` edits) with
   a **stage-0 crutch guard**: an in-process `path → sha` map inside the
   plugin instead of the doc-19 `fs.seen` projection — functionally the
   same two checks (no prior read / stale read), replaced by the honest
   projection once reflex has built doc 19 itself.
3. **`go.build` / `go.test` / `go.vet` plugins** — deterministic
   wrappers, the validators. Their results in the fold are what the
   operator (and later a gate) judges work by.
4. **The operator surface**: send a task (`reflex emit --daemon`,
   shipped) and print a request's fold (`SessionProjection`, shipped).
   Committing is done by the operator, after reviewing the fold and the
   diff — the approval gate lives outside the topology at stage 0.

Deliberately **not** hand-written: docs 16/17/19/20 machinery, gates,
subagents, repair lanes, the remaining model adapters. All of it is
tasks.

## Safety: no bash, no self-wiring

Two hard rules, both already latent in the model and made explicit here:

- **No bash tool.** Doc 14's stance — deterministic single-purpose tools
  instead of a shell — is a *safety boundary* for a self-hosting agent,
  not just a design preference. The agent's entire action surface is the
  named plugins above, each rooted and single-purpose; there is no
  generic process execution, no network tool, no package installation.
  A new capability enters only as a new single-purpose plugin, wired by
  an operator changeset. (Corollary for doc 14's open "search backend"
  question: in-process, definitively — shelling to `rg` would smuggle a
  process spawn inside the no-bash boundary.)
- **No self-wiring.** The agent writes code; only the operator wires
  topology. Changeset principals (docs 05/20) carry this: the brain has
  no emit rights on `sys.topology.*` kinds, so the agent cannot
  subscribe itself — or anything else — anywhere. Self-improvement
  always crosses a human boundary twice: code lands via reviewed
  commits, wiring lands via operator changesets. (A later relaxation,
  if ever, is the doc-18 confirm pattern: the agent emits a *proposed*
  changeset, a gate requires the operator's typed approval.)

## The self-build order

Sequenced by "what improves the builder fastest", not by phase numbers:

1. **Calibration task: the Gemini and OpenAI-compatible adapters.**
   Additive Go code, obvious tests, immediately useful (cheap models for
   future lanes) — and it calibrates the task size the agent can carry.
2. **Obligation counting (17) + node-rooted scopes (16).** On landing,
   the agent's own topology migrates: pumps die, `scope.brain.closed`
   appears, parallel tool calls become possible.
3. **Projections (19)** — the walk evaluator, `reads:` / attach. The
   real transcript view; the fs plugin's crutch guard is replaced by
   `fs.seen`.
4. **Changesets (20).** From here the operator improves the running
   agent live, and doc 21's ladder starts in earnest: the verification
   gate, the explore subagent on a cheap model, repair lanes.
5. **Onward**: budgets / deadlines / cancellation (16), context
   compaction (21 gap #1), G5 frontier rebuild, the rest of the roadmap —
   the ladder rising in parallel with the engine.

**The rule of the game:** as soon as the agent lands a feature its own
topology can use, the next changeset moves the agent onto it. The
migration *is* the feature's acceptance test — dogfood as validation.

## Constraints this surfaces (beyond doc 21's gaps)

1. **The self-hosting restart problem.** The agent edits source; the
   *old* binary is what runs it. Rebuild + restart happens between
   tasks, and only at quiescence — because G5 (frontier rebuild from the
   log) is not implemented (doc 16, delta #2). Fine at stage 0, where a
   task is one request; G5 enters the self-build queue exactly when
   tasks outgrow single requests.
2. **The commit gate.** "Validated → committed" is a human at stage 0.
   The growth path is written: the doc-18 human-in-the-loop pattern
   (confirm as a typed result) plus a `git.commit` plugin — which the
   agent does *not* get until that gate exists in topology.
3. **Task sizing is the operator's main skill at the start.** Until the
   agent builds itself context compaction (21 #1), task size is bounded
   by the model window. The mitigating fact: the per-doc deltas lists
   are already nearly window-sized — the roadmap pre-sliced itself.

## Open / unresolved

- **When (if ever) the agent gets `git.commit`.** Lean: after the
  verification gate (21, ladder step 2) plus the confirm-gated commit
  flow; never ungated.
- **When (if ever) the agent gets changeset rights.** Lean: never
  directly — proposed-changeset + operator approval is strictly better
  and is one gate node.
- **Stage-0 model choice.** A strong model (Vertex-Anthropic) for the
  brain from day one vs starting cheap and measuring. Lean: strong —
  stage 0's bottleneck is task success rate, not cost.
- **Where stage-0 task definitions live.** Plain operator prompts vs a
  task file the context-loader folds in. Lean: prompts first; structure
  emerges when the agent builds its own context loading (ladder step 6).
