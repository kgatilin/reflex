# 25 — Regulation: the target, and the sufficiency of the primitives

> **Status: CONCEPT / forward-looking.** The picture reflex regulation
> wants to arrive at — a self-balancing agent system governed by a single
> conserved quantity ("energy") — and the proof that building it needs **no
> fourth primitive**. The staging is explicit: **for now the regulator is
> an external agent**; reflex's only job is to expose a sufficient
> read-and-write surface so that regulation *can* be driven from outside
> today and *crystallised* inward later (doc
> [08](./08-optimization-as-rewrite.md)/[24 §8](./24-concept.md)) without a
> grammar change. This document is the sufficiency check, not a build
> order. It adds nothing to the settled model of
> [24](./24-concept.md) — it spends it.

---

## 1. The target picture

A deployed agent system is **cost-bound**, not throughput-bound: every
action depletes a finite pool of real money (LLM tokens, standing
infrastructure), and the system must stay solvent over time. The regulation
target frames this as one conserved quantity:

```
E(t) ≥ 0      a single reservoir, per instance (per-tenant deferred — doc 24 §C)
dE = inflow − outflow
  inflow  ← positive human feedback, external funding, passed verifications
  outflow ← llm.usage, standing subscriber cost, negative feedback, stalls
```

Regulation is the loop that keeps E solvent by trading **width against
depth** under the budget identity `N_subscribers × cost_per_subscriber ≤
E_avg`: many cheap nodes *or* few expensive nodes, never many expensive
nodes. When E runs low the regulator **degrades** (cheaper models, less
parallelism, stop admitting new work); when E is abundant it **grows**
(upgrade models, spawn experiments). The same E grounds the fitness
function for any evolutionary optimiser over topology (a strictly larger
search space than harness- or skill-level self-improvement — left to a
later doc; this one establishes the substrate it would run on).

Two framings worth keeping straight:

- **This is the economic/dollar budget, not the context-token budget.** The
  largest open hole ([24 §A.1](./24-concept.md)) — per-request token window
  and view compaction — is a *different* axis. Energy keeps the *instance*
  solvent; it does not bound a single request's prompt growth. Do not
  conflate.
- **E is a soft control target, not a hard structural invariant.** The hard
  per-request bound is the engine budget (G7); E is what a controller
  *tracks*. Why this is not a weakness is §6.

## 2. The sufficiency claim

> Every regulatory action reduces to writing one of three control-plane
> facts over a read surface of projections. There is no fourth verb, and
> none of the three is a new primitive.

| Verb | Mechanism | Source |
|---|---|---|
| **config-fact** | `sys.state.updated.node.{name}.config{…}` — whole-object behavioural swap (model, prompt, params, allowlist) | [24 §A.4](./24-concept.md) |
| **changeset-request** | `sys.topology.changeset.requested{ ops, principal }` — wiring change, validated as a resulting graph | [20](./20-topology-management.md) |
| **intervention** | `sys.scope.{instance}.cancelled` / `.budget.extended` / `.deadline.extended` — target a live cone | [20](./20-topology-management.md) |

Enforcement is **internal and already exists**: the `llm` body reads its
config kv at the trigger's log position; the engine refuses dispatch on an
exhausted budget; the resolver honours an admission-policy fact. The
regulator never enforces — it writes facts, and the standing machinery
acts on them. That separation is what lets the regulator live *outside* the
runtime (§5).

The rest of this document constructs the regulation agent piece by piece
and shows each piece landing on this surface.

## 3. The reservoir is a reaction, not a projection

A running sum is neither shape the projection grammar offers (`kv` /
`log`, [19](./19-projections.md)/[24 §6](./24-concept.md)). The doc-24
escape hatch applies verbatim — *"anything richer is a reaction: subscribe,
compute, emit `state.updated.{path}`; the generic kv serves the result."*

```yaml
# EnergyAccountant — an llm-less reaction (pure fold body)
reactions:
  - name: energy.accountant
    on:    [llm.usage, tool.*.result, feedback.received, funding.received]
    reads: [pricing, energy]          # kv views, see below
    emit:  [sys.state.updated.energy]
```

- It folds the running balance and emits `sys.state.updated.energy{ value }`
  (terminal, whole-object).
- **E is a `sys.` fact, deliberately.** Session-less machinery, so the kv
  fold over `sys.state.updated.energy` is global *by construction* — the
  exact mechanism the LLM tool menu already uses (kv over `sys.subscribed`,
  [24 §A.4](./24-concept.md)). This sidesteps needing `in: global` on the
  reservoir at all; the reservoir is outside every cone because it *is*
  control-plane state.
- The accountant reads payloads freely. Domain-blindness is an **engine**
  property (dispatch/closure/stamping read structure only,
  [24 §4](./24-concept.md)); a reaction is domain code —
  `React(event, views) → events` — and may sum `tokens × rate` from a
  payload without violating anything.

**Cost is derived, never stored.** The reservoir does not require new
event fields. `llm.usage` already carries tokens (stage 0, doc
[23](./23-bootstrap-roadmap.md)); the accountant multiplies by a rate read
from a `pricing` config-fact. The log stays neutral — doc
[22](./22-bootstrap-self-hosting.md)/[23](./23-bootstrap-roadmap.md)'s
position that a neutral cost log does not obstruct later levers holds. Do
not add `energy_cost` to the envelope.

## 4. The levers, each a fact

**Inflows arrive as events.** `feedback.received{ polarity, magnitude,
source }` is produced by a `feedback.interpreter` reaction converting
surface signals — Slack reaction, billing webhook — through the standard
ingress→resolver path (`app.ingress.{surface}.{event}` →
[24 §2](./24-concept.md)). `funding.received` is a `sys.`-level injection.
Nothing here is more than a typed event.

**Threshold crossings announce themselves.** When the balance crosses a
configured low/high mark the accountant (or a thin monitor over the kv)
emits `sys.energy.depleted` / `sys.energy.abundant`. This is precisely the
doc-24 rule *"structural threshold crossings are announced back onto the
log as events"* ([24 §1](./24-concept.md)) — a projection boundary becoming
a fact.

**Degrade and grow are config-facts.** A controller reacting to
`sys.energy.depleted` emits `sys.state.updated.node.brain.config{ model:
haiku, … }` — one atomic whole-object swap. The `llm` body reads its config
kv at the next firing's trigger position and is now cheap. No changeset:
wiring is unchanged, only behaviour ([24 §A.4](./24-concept.md)). Growth is
the mirror — upgrade configs the same way, or spawn experimental nodes via
a changeset (operator-gated, §7).

**Mid-flight cost is an intervention.** A cone already running too
expensively is cancelled with `sys.scope.{instance}.cancelled`; partial
progress is recorded (the log keeps what happened) and the resolver chains
any follow-up through the cancelled closure
([20](./20-topology-management.md), [24 §5](./24-concept.md)).

**Admission is a policy fact, enforced at the resolver.** The hard "stop
taking new work when E ≤ 0" lives at the **resolver**, not the dispatcher.
The regulator writes `sys.state.updated.admission.policy{ min_energy }`; the
resolver — already the reaction that maps ingress to a session and mints
`request.received` ([24 §2](./24-concept.md)) — reads E and the policy, and
on deficit emits `app.ingress.{surface}.refused{ reason: energy }` instead
of minting the request. This is the note's "gate at the bus" instinct
placed one layer up: admission control is a domain reaction reading a
projection; it is *not* a payload branch inside dispatch (which the
anti-catalog forbids, [24 §4](./24-concept.md)). Refusing to admit, rather
than killing in-flight work, is also the honest behaviour — it is the
hibernation the regulation target already calls for.

## 5. The regulator is external — and that is expressible today

For v1 the entire control loop runs **outside** the runtime, as a separate
principal:

```
            reflex runtime                         external regulator
   ┌──────────────────────────────┐        ┌────────────────────────────┐
   │ projections (read surface):  │ ─────► │ observe:                   │
   │   energy (kv/sys)            │        │   reservoir, fitness folds,│
   │   cost folds (llm.usage)     │        │   trace corpus             │
   │   request.handled / failed   │        │ decide (any logic, any     │
   │   trace corpus (log views)   │        │   model, offline)          │
   └──────────────────────────────┘        │ act, via the three verbs:  │
   ┌──────────────────────────────┐ ◄───── │   config-fact              │
   │ control plane (write surface)│        │   changeset.requested      │
   │   the three verbs of §2      │        │   scope intervention       │
   └──────────────────────────────┘        └────────────────────────────┘
```

- **Read** is the embedding API / daemon over declared projections
  ([09](./09-embedding-api.md), [19](./19-projections.md)): the reservoir,
  the cost fold, the success/failure folds, and trace views. No query RPC —
  every read is a declared fold, replay-reproducible.
- **Write** is the control plane ([05](./05-control-plane-as-events.md),
  [20](./20-topology-management.md)): the regulator is a principal holding
  scope grants ([06](./06-permissions-and-scopes.md)), and *only the engine
  writes facts* in response to its `*.requested` events. A regulator cannot
  claim a subscription or a config into existence; it proposes, the engine
  validates and records.
- **Reflex does not know regulation exists.** The accountant, the
  controllers, the admission policy — for v1 these can equally be the
  external agent's emissions rather than in-bus reactions. The runtime sees
  ordinary events from an authorised principal. This is the whole point of
  the no-privileged-plane invariant (G8): the regulator sits on the same
  log as everything else, fully audited, with no special channel.

The crystallisation path is then mechanical: when the external loop's
behaviour is regular, compile each controller into an in-bus reaction by
changeset ([08](./08-optimization-as-rewrite.md),
[24 §8](./24-concept.md)). The internal and external regulators are the
same events from a different emitter; moving the boundary is a deployment
change, not a model change.

## 6. Two layers: why soft E is not a hole

The regulation target says "actions gated on E ≥ 0." The model splits this
into two guarantees that must not be confused:

- **Hard, structural, per-cone:** engine budgets (G7). A mandatory default
  budget on the session scope makes *every request* bounded; `scope.budget_low`
  warns one step early, `scope.budget_exhausted` refuses dispatch. This is
  structure-only (per-kind counts + wall clock) and lives in the engine.
- **Soft, economic, global:** the energy controller. It tracks E toward a
  set point and steers *configuration* (which model, how many nodes,
  whether to admit). Stability is a control-theory property of the loop
  (a Lyapunov/integral-feedback argument), plus the admission gate of §4 —
  **not** a structural invariant.

There is deliberately **no global structural hard-stop**. Scopes root on
events; the session is the top scope ([24 §2](./24-concept.md)); a
"global budget" would require the dispatcher to consult a domain view
before every delivery — a payload branch, forbidden. So global solvency is
held by the controller plus admission, and the failure mode under a bad
feedback spiral is *degradation and hibernation*, not a hard kill of
in-flight requests. For a cost-bound system serving real users this is the
correct behaviour, not a compromise.

## 7. Design constraints the target must respect

Three places where the regulation framing, taken literally, would reach for
engine magic the model refuses. In each, the model forces a cleaner
placement:

1. **No energy gate inside dispatch.** Summing per-event cost and refusing
   delivery in the dispatcher inspects payloads — anti-catalog
   ([24 §4](./24-concept.md)). The hard bound is the per-cone budget; the
   global bound is the admission reaction (§4). Right idea, wrong layer.
2. **No "dispatcher as auctioneer."** Routing expensive nodes to
   high-value events is value-based dispatch — again a payload branch.
   Value routing is a **router reaction** (a data-fork, [15](./15-primitive-reduction.md)):
   it reads an event's value from a projection and emits onward. Domain
   code, not the engine.
3. **No autonomous self-wiring.** Topology mutation crosses the human
   boundary doc [22](./22-bootstrap-self-hosting.md) erects on purpose
   ("self-improvement crosses a human boundary twice"). The regulator may
   evolve **behaviour** autonomously (config-facts — model/prompt/params,
   the "A/B = one event" axis) but may change **wiring** only as a
   proposed changeset under operator approval, or offline. The
   external-regulator staging of §5 makes this boundary literal: the
   regulator is a principal whose changeset rights are granted, reviewed,
   and revocable.

## 8. What the target leans on that is not yet built

Neither is a new primitive; both are already-named gaps.

- **`in: global` horizon** ([24 §A.4/§B](./24-concept.md)). The reservoir
  avoids it by living as a `sys.` fact (§3). But any *domain-level* fold a
  controller wants across all sessions without routing through the sys
  plane needs the global, log-order horizon the projection grammar still
  lacks. Energy is the third independent motivation for it (after node
  config and the tool menu) — which strengthens the lean rather than adding
  to it.
- **The fork operation** ([24 §A.5](./24-concept.md)): "fork a log at a
  position under a different topology." The evolutionary-optimiser future
  of §1 needs it as its evaluation harness, and it is the same operation
  the operator's regression loop needs. Building it once serves both.

## 9. Sufficiency verdict

Run the standing test ([24 §D](./24-concept.md)) over the whole regulation
agent — *is any piece a new primitive, or all conventions over
Event / Reaction / Projection?*

| Regulation piece | Reflex construct | New primitive? |
|---|---|---|
| Reservoir E | accountant reaction + kv over `sys.state.updated.energy` | no |
| Cost accounting | derived from `llm.usage` × pricing config-fact | no |
| Inflows (feedback/funding) | ingress → resolver → typed events | no |
| Threshold signals | announce-on-log crossing | no |
| Degrade / grow | `sys.state.updated.node.*.config` (verb 1) | no |
| Mid-flight throttle | `sys.scope.*.cancelled` (verb 3) | no |
| Global admission gate | admission-policy fact + resolver enforcement | no |
| Value routing | router reaction (data-fork) | no |
| Per-request hard bound | engine budget (G7) | no |
| Fitness for evolution | existing folds (`request.handled`, cost, judge) | no |

Every row lands. The two open dependencies (§8) are grammar/tooling the
canon already leans toward, not primitives. The only thing the model
refuses outright is the *placement* the naive framing assumed — energy
logic inside the dispatcher — and in each case the refusal yields a
cleaner design (admission at the resolver, value routing as a reaction,
wiring behind the operator).

**Conclusion: the mechanisms are sufficient.** Regulation is buildable from
the settled primitives, and is drivable today by an external agent over the
projection read surface and the three-verb write surface, with no new
concept entering the model. Internalising the regulator later is a
crystallisation changeset, not a redesign.

## See also

- [24-concept.md](./24-concept.md) — the settled model this spends; §A.4
  (config-as-facts), §5 (scopes/budgets/cancellation), §6 (projection
  shapes), §8 (crystallisation), §D (the standing test).
- [20-topology-management.md](./20-topology-management.md) — the changeset
  pipeline and per-instance interventions the regulator writes through.
- [19-projections.md](./19-projections.md) — the read surface; `in:`
  horizons and the `state.updated.>` kv convention.
- [09-embedding-api.md](./09-embedding-api.md) — how an external principal
  reads projections and posts control-plane events.
- [08-optimization-as-rewrite.md](./08-optimization-as-rewrite.md) — the
  optimiser-as-changeset-client position the internalised regulator would
  inherit.
- [23-bootstrap-roadmap.md](./23-bootstrap-roadmap.md) — `llm.usage` →
  `reflex costs`, the cost surface the reservoir folds.
