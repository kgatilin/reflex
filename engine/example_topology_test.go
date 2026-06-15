package engine

// exampleTopology encodes the doc-27 §3/§4 subscriber list as a []Decl: the
// simple coding agent (resolver, understand [read-only], gather, plan,
// execute, fs, gotool, the two loop guards, notify) plus the global/request
// projections that fold state.updated facts into views.
//
// Modelling notes that keep the healthy topology connected:
//   - Tool nodes subscribe to tool.{name}.> so a single fs node consumes both
//     tool.fs.read.call and tool.fs.search.call (the subcalls understand/gather
//     emit), matching "fs (tool) on: tool.X.call" with the real subject space.
//   - The work region is one cyclic SCC (gather/plan/execute loop through the
//     tools). Termination is a scope property, not a node property (doc 24 §5
//     "loops are budgets"): the "request" scope carries a Budget bounding the
//     per-kind tool-call count within the cone, and every node in the SCC sits
//     within it, so the validator reports no unbounded cycle.
//   - State-field updates (state.updated.*) are consumed by projections, not
//     by node subscriptions — folding a fact into a view IS consuming it, so
//     they are not dead-ends.
func exampleTopology() []Decl {
	return []Decl{
		// request scope: rooted by request.received (resolver), Budget bounds the
		// loop kinds within the cone so the work SCC terminates (doc 24 §5).
		Scope{
			Name: "request",
			Root: "request.received",
			Budget: map[string]int{
				"tool.fs.read.call":      32,
				"tool.fs.search.call":    32,
				"tool.gotool.build.call": 32,
			},
		},
		// resolver: external entry (cli.task) → request.received (roots the request scope).
		Subscriber{
			Name:  "resolver",
			On:    []string{"cli.task"},
			In:    "global",
			Emits: []string{"request.received"},
			Scope: "request",
		},
		// understand (llm, read-only): turns the request into a goal and the
		// first read/search tool calls.
		Subscriber{
			Name: "understand",
			On:   []string{"request.received"},
			In:   "request",
			Emits: []string{
				"state.updated.goal",
				"state.updated.status",
				"tool.fs.read.call",
				"tool.fs.search.call",
			},
		},
		// gather (llm): consumes fs results, loops (another read) or exits via
		// plan.requested / task.needs_clarification.
		Subscriber{
			Name: "gather",
			On: []string{
				"tool.fs.read.result",
				"tool.fs.search.result",
				"tool.fs.read.failed",
				"tool.fs.search.failed",
			},
			In: "request",
			Emits: []string{
				"state.updated.context.found",
				"sys.state.updated.project.context.found",
				"state.updated.sufficiency",
				"tool.fs.read.call",
				"plan.requested",
				"task.needs_clarification",
			},
		},
		// gather-guard: an extra consumer on the re-trigger edges that can emit
		// the exit (plan.requested). It is an ordinary SCC member; the loop is
		// bounded by the request scope's budget, not by this node (doc 24 §5).
		Subscriber{
			Name: "gather-guard",
			On: []string{
				"tool.fs.read.result",
				"tool.fs.search.result",
			},
			In:    "request",
			Emits: []string{"plan.requested"},
		},
		// plan (llm): writes the plan and flips status to executing.
		Subscriber{
			Name: "plan",
			On:   []string{"plan.requested"},
			In:   "request",
			Emits: []string{
				"state.updated.plan",
				"state.updated.status",
			},
		},
		// execute (llm): runs steps via tools; loops on results; answers when
		// the plan is complete.
		Subscriber{
			Name: "execute",
			On: []string{
				"state.updated.plan",
				"tool.gotool.build.result",
				"tool.fs.read.result",
				"tool.gotool.build.failed",
				"tool.fs.read.failed",
			},
			In: "request",
			Emits: []string{
				"tool.gotool.build.call",
				"tool.fs.read.call",
				"state.updated.plan.0.status",
				"state.updated.status",
				"task.answered",
			},
		},
		// execute-guard: an extra consumer that can emit task.answered. Like
		// gather-guard, it is a plain SCC member; the budget bounds the loop.
		Subscriber{
			Name:  "execute-guard",
			On:    []string{"tool.gotool.build.result"},
			In:    "request",
			Emits: []string{"task.answered"},
		},
		// fs (tool): tool.fs.* island. In the request scope so it is inside the
		// budgeted cone that bounds the work SCC (doc 24 §5).
		Subscriber{
			Name: "fs",
			On:   []string{"tool.fs.>"},
			In:   "request",
			Emits: []string{
				"tool.fs.read.result",
				"tool.fs.search.result",
				"tool.fs.read.failed",
				"tool.fs.search.failed",
			},
		},
		// gotool (tool): tool.gotool.* island. In the request scope for the same
		// reason as fs — inside the budgeted cone (doc 24 §5).
		Subscriber{
			Name: "gotool",
			On:   []string{"tool.gotool.>"},
			In:   "request",
			Emits: []string{
				"tool.gotool.build.result",
				"tool.gotool.build.failed",
			},
		},
		// notify (sink): consumes the two terminal answers, replies to the
		// user. Emits nothing — a legitimate sink, not a dead-end.
		Subscriber{
			Name: "notify",
			On: []string{
				"task.answered",
				"task.needs_clarification",
			},
			In: "request",
		},
		// lifecycle (sink): consumes scope.request.closed — the request cone's
		// closure is where the OTel trace ends and the audit fold runs (doc 24
		// §5). It is the deterministic terminator the stalled-closure check
		// requires (doc 26 §3f / 27 §5): without a consumer, a request that
		// quiesces on a non-terminal state would freeze in the void. It lives in
		// the parent (global) cone — closure exits the cone it seals — so it is
		// scope-less (In defaults to global).
		Subscriber{
			Name: "lifecycle",
			On:   []string{"scope.request.closed"},
		},

		// Projections — the views nodes read; they consume state.updated facts.
		Projection{
			Name:  "project_context",
			On:    []string{"sys.state.updated.project.context.found"},
			In:    HorizonGlobal,
			Type: TypeKV,
		},
		Projection{
			Name: "task_context",
			On: []string{
				"state.updated.goal",
				"state.updated.context.found",
				"state.updated.sufficiency",
				"state.updated.status",
			},
			In:    HorizonRequest,
			Type: TypeKV,
		},
		Projection{
			Name: "plan_view",
			On: []string{
				"state.updated.plan",
				"state.updated.plan.>",
			},
			In:    HorizonRequest,
			Type: TypeKV,
		},
	}
}
