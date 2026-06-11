# 17 — Quiescence mechanics: lessons from Timely/Naiad progress tracking (DRAFT)

> **Status: DRAFT / reading notes turned normative.** Prior-art study for
> delta #1 of [16-engine-architecture.md](./16-engine-architecture.md)
> (per-scope incremental quiescence). Sources: the Naiad paper (Murray et
> al., SOSP 2013, §2.3–3.3) and the mechanised verification of its progress
> tracking protocol (Brun, Decova, Lattuada, Traytel, *Verified Progress
> Tracking for Timely Dataflow*, ITP 2021, Isabelle/HOL). The mapping yields
> three normative corrections to doc 16, listed at the end.

## Why this prior art

`scope.closed` (doc 15/16) is reflex's barrier: a notification that a cone
has quiesced, delivered exactly once (G6). Timely dataflow's `OnNotify(t)`
is the same guarantee in a different geometry — "no further messages with
timestamp ≤ t will arrive" — and its progress tracking protocol is the only
production mechanism for this that has survived formal verification. Before
implementing per-scope quiescence, steal what transfers and understand what
doesn't.

## The protocol in five pieces

1. **Pointstamps and occurrence counts.** Every unit of pending work is a
   pointstamp `(t, location)` — a message on an edge or a notification
   request at a vertex. The scheduler keeps, per active pointstamp, an
   **occurrence count** (how much undone work bears it): `SendBy`/`NotifyAt`
   increment, completed `OnRecv`/`OnNotify` decrement.
2. **Precursor counts and the frontier.** Per active pointstamp, a
   **precursor count**: how many other active pointstamps could-result-in
   it. Precursor count zero ⇒ the pointstamp is in the **frontier**, and
   only then may its notification be delivered. This is the `OnNotify`
   guarantee — the analogue of G6.
3. **Could-result-in via path summaries.** `(t₁,l₁)` could-result-in
   `(t₂,l₂)` iff the minimal path summary Ψ[l₁,l₂] over the static graph
   satisfies `Ψ[l₁,l₂](t₁) ≤ t₂`. Cycles stay tractable because timestamps
   carry loop counters — `(epoch, ⟨c₁…cₖ⟩)` — incremented at feedback
   vertices, so "later around the loop" is order-visible.
4. **The API monotonicity invariant.** A callback invoked with timestamp
   `t` may only `SendBy`/`NotifyAt` with `t' ≥ t` — no messages backwards in
   time. Every lower bound the protocol computes rests on this.
5. **The distributed exchange ("clocks protocol") and uprightness.**
   Workers hold *local* occurrence/precursor counts and broadcast deltas
   `(p, δ)` FIFO per worker pair; safety: no local frontier ever runs ahead
   of the global one. The verified core invariant is **uprightness**: a
   change-multiset Δ is admissible only if every positive entry (new
   pointstamp) is accompanied by a *smaller* negative entry (a held/retired
   one) — work can only be introduced from work already held. The ITP
   paper's headline finding: **Naiad's API admitted non-upright
   transitions** (an operator could keep sending from `OnNotify` without
   decrementing a held pointstamp), so the previously proven protocol model
   (Abadi et al., TLA⁺) did not cover the implementation's actual
   behaviour. The divergence surfaced only under mechanised verification.

## What reflex does NOT need, and why

| Timely mechanism | Needed? | Reason |
|---|---|---|
| could-result-in / path summaries | no | timely must *predict* which timestamps can still arrive over a static cyclic graph; a reflex cone is direct `caused_by` ancestry — membership is observed, never predicted |
| loop counters in timestamps | no | a capped subscription cycle unrolls into *fresh DAG nodes* each iteration; the causal graph is acyclic by construction. The complexity timely puts into the timestamp lattice, reflex puts into the graph itself |
| the distributed exchange protocol | no, while the log has one writer | a single appender makes the global counts exact; there are no local approximations to reconcile. **The single-writer log buys back roughly 80% of the protocol — the entire part that needed an ITP paper.** Federating the log writer is the deliberate entry into that zone, never an incidental scaling step |

The trade is symmetrical and worth stating: timely tracks progress over a
*static cyclic* graph with *structured* timestamps; reflex tracks progress
over a *dynamic acyclic* graph with *trivial* timestamps (event ids). The
cost reflex pays instead is an unboundedly growing DAG — which is why the
frontier must be maintained incrementally (counts), never recomputed by
cone traversal.

## What reflex must take

### 1. Obligation counts — the frontier definition in doc 16 was incomplete

Doc 16 defined `frontier(cone) = non-terminal events with no descendants`.
As an *online mechanism* this is broken, in a way the Naiad lens makes
obvious: it cannot distinguish

- a non-terminal leaf whose handlers are **still executing** (cone must
  stay open), from
- a non-terminal leaf **all of whose handlers completed emitting nothing**
  (an orphan — the cone should close and G4 should announce it).

Worse, it is circular with G4: the watcher announces orphans
*post-quiescence*, but under the naive definition an orphan blocks
quiescence forever.

The fix is exactly Naiad's occurrence count, recast as **obligation
counting**:

```
dispatch of event E to N subscribers      → cone obligations += N
one handler completes (emits appended)    → obligations += |emits dispatched|, then -= 1
quiescent(scope)                          ⇔ obligations(scope) == 0
```

An event whose last handler decrements to zero leaving it non-terminal and
descendant-less is **detected as orphaned at that instant** — the orphan
check is the zero-crossing of its own counter, not a post-hoc DAG sweep.
Quiescence and orphan detection are one mechanism observed at two
granularities (per event, per scope root).

For out-of-process tools the rule is: the obligation opened by dispatching
`tool.x.call` closes when the **result event is appended**, not when the
call is delivered — a pending plugin call holds its cone open. (This is
timely-rust's *capability* concept: the right to still produce output at a
timestamp, held across asynchronous execution.)

Nesting (transitive quiescence) is the same counters maintained per scope
root: each append/complete updates every scope root on the event's
ancestor chain — O(nesting depth) per event, online, no traversal.

### 2. Uprightness ⇒ `caused_by` is engine-stamped, never handler-chosen

Reflex's analogue of uprightness: **an event may only be introduced from a
cause currently being processed.** The synchronous `React → []events`
contract gives this by construction — the engine appends the children and
retires the trigger in one step; that atomicity *is* the upright
transition.

The hazard is any future API that lets a handler choose its own
`caused_by`: it could emit into a cone *after* that cone's `scope.closed`
fired, retroactively falsifying G6 — precisely the class of bug
uprightness exists to exclude. Hence the rule, same as for `request_id`:
the dispatcher is the sole writer of `caused_by`. A handler names nothing;
its emits are children of its trigger, full stop. Asynchronous emitters
(plugins) emit through a held obligation (the pending call), which plays
the role of the "smaller negative entry" — they extend the cone through a
live cause, never reach around it.

### 3. Make violations inexpressible, not forbidden

The ITP finding generalises: Naiad's protocol model and its API diverged
silently, and only mechanised verification caught it. The transferable
lesson is not "verify everything" but **close the gap by construction**:
the invariants above are carried by the API shape (synchronous return,
engine-stamped causality, obligation-scoped async emission) rather than by
handler discipline. A reflex handler should have no expressible way to
produce a non-upright transition.

## Algorithm sketch for delta #1

Per declared-or-auto-rooted scope root `R`, the engine maintains:

```
obl[R]    int     open obligations in scope(R)            (counts, §1 above)
```

- **append(E)**: for each scope root on E's ancestor chain (nearest first,
  stopping at the session root): nothing yet — appending alone creates no
  obligation.
- **dispatch(E → N subscribers)**: `obl[R] += N` for each ancestor root R.
  N = 0 and E non-terminal ⇒ E is orphaned immediately (`event.orphaned`).
- **handler-complete(E, emits)**: append + dispatch the emits (their own
  increments), then `obl[R] -= 1` for each ancestor root. On any
  decrement to zero: emit `scope.closed{root: R}` with `caused_by` = the
  cone's current leaves; evaluate `closed_when` predicates other than
  `quiescent` at every counter change.
- **plugin call**: the dispatch increment stays open until the result/
  failed event is appended (capability), then decrements.

Cost: O(nesting depth) counter updates per event; no DAG traversal on the
hot path; the cone fold happens only when a consumer of `scope.closed`
asks for it.

## Corrections this implies in doc 16

1. The frontier/quiescence definition gains the obligation-count precision
   (the descendant-less formulation stays correct as the *log-level* view;
   the *operational* view counts obligations).
2. G4's wording — orphan detection is the zero-crossing, not a
   post-quiescence sweep; the watcher consumes these crossings rather than
   discovering them.
3. New normative rule alongside the `request_id` stamping rule:
   `caused_by` is dispatcher-stamped (uprightness).

## Sources

- D. Murray et al., *Naiad: A Timely Dataflow System*, SOSP 2013 —
  §2.3 (pointstamps, occurrence/precursor counts, frontier), §3.3
  (distributed protocol, broadcast optimisations).
- M. Brun, S. Decova, A. Lattuada, D. Traytel, *Verified Progress Tracking
  for Timely Dataflow*, ITP 2021 — the clocks protocol re-formalisation,
  the uprightness invariant, the Naiad API divergence finding; AFP entry
  `Progress_Tracking`.
- M. Abadi et al., TLA⁺ formalisation of the exchange protocol (via ITP
  paper §3).
