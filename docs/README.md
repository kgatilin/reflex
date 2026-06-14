# reflex design documentation

**Start here:** [`CONCEPT.md`](./CONCEPT.md) — the single canonical description of
the current model, what is built, and the language/conventions. Read it first;
when an older doc disagrees, `CONCEPT.md` wins.

## Current docs

| Doc | Role |
|---|---|
| [`CONCEPT.md`](./CONCEPT.md) | **Canonical.** The settled model, implementation status, language, open questions. |
| [`26-bare-substrate.md`](./26-bare-substrate.md) | The "why" for the substrate: scope as a projection, termination as a budget, events-not-tools, the catalog, view types. |
| [`27-state-defined-agent.md`](./27-state-defined-agent.md) | The "why" for the agent: state model → subscriber list → engine-validated connectivity. |
| [`28-substrate-rebuild-roadmap.md`](./28-substrate-rebuild-roadmap.md) | **LIVE** build tracker — what shipped, what is next. |
| [`29-swebench-target.md`](./29-swebench-target.md) | **TARGET** — one SWE-bench Lite instance on the new stack: the goal, the agent topology, the framework plan. |

## History

[`outdated/`](./outdated/) holds the full design journey (docs 00–25): the
legacy handler/eight-node era, the primitive-reduction path, and the prior
consolidation [`outdated/24-concept.md`](./outdated/24-concept.md) that
`CONCEPT.md` supersedes. Kept for the "why" and the decision trail; **superseded
in vocabulary and several mechanisms** by the current docs above. Do not treat
anything in `outdated/` as describing the current model.
