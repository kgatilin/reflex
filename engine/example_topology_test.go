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
//   - The two loops carry a deterministic *-guard subscribed to the same
//     re-trigger edges (doc 27 §3 "*-guard (deterministic)"): this is the node
//     that makes the work SCC bounded.
//   - State-field updates (state.updated.*) are consumed by projections, not
//     by node subscriptions — folding a fact into a view IS consuming it, so
//     they are not dead-ends.
func exampleTopology() []Decl {
	return []Decl{
		// resolver: ingress → request.received (roots the request scope).
		Node{
			Name:  "resolver",
			Kind:  KindDeterministic,
			On:    []string{"app.ingress.*"},
			In:    "global",
			Emits: []string{"request.received"},
			Scope: "request",
		},
		// understand (llm, read-only): turns the request into a goal and the
		// first read/search tool calls.
		Node{
			Name: "understand",
			Kind: KindLLM,
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
		Node{
			Name: "gather",
			Kind: KindLLM,
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
		// gather-guard (deterministic): same re-trigger edges; forces the exit
		// (plan.requested) at budget. The deterministic node in the work SCC.
		Node{
			Name: "gather-guard",
			Kind: KindDeterministic,
			On: []string{
				"tool.fs.read.result",
				"tool.fs.search.result",
			},
			In:    "request",
			Emits: []string{"plan.requested"},
		},
		// plan (llm): writes the plan and flips status to executing.
		Node{
			Name: "plan",
			Kind: KindLLM,
			On:   []string{"plan.requested"},
			In:   "request",
			Emits: []string{
				"state.updated.plan",
				"state.updated.status",
			},
		},
		// execute (llm): runs steps via tools; loops on results; answers when
		// the plan is complete.
		Node{
			Name: "execute",
			Kind: KindLLM,
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
		// execute-guard (deterministic): forces task.answered at budget.
		Node{
			Name:  "execute-guard",
			Kind:  KindDeterministic,
			On:    []string{"tool.gotool.build.result"},
			In:    "request",
			Emits: []string{"task.answered"},
		},
		// fs (tool): tool.fs.* island.
		Node{
			Name: "fs",
			Kind: KindTool,
			On:   []string{"tool.fs.>"},
			In:   "global",
			Emits: []string{
				"tool.fs.read.result",
				"tool.fs.search.result",
				"tool.fs.read.failed",
				"tool.fs.search.failed",
			},
		},
		// gotool (tool): tool.gotool.* island.
		Node{
			Name: "gotool",
			Kind: KindTool,
			On:   []string{"tool.gotool.>"},
			In:   "global",
			Emits: []string{
				"tool.gotool.build.result",
				"tool.gotool.build.failed",
			},
		},
		// notify (sink): consumes the two terminal answers, replies to the
		// user. Emits nothing — a legitimate sink, not a dead-end.
		Node{
			Name: "notify",
			Kind: KindDeterministic,
			On: []string{
				"task.answered",
				"task.needs_clarification",
			},
			In: "request",
		},

		// Projections — the views nodes read; they consume state.updated facts.
		Projection{
			Name:  "project_context",
			On:    []string{"sys.state.updated.project.context.found"},
			In:    HorizonGlobal,
			Shape: ShapeKV,
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
			Shape: ShapeKV,
		},
		Projection{
			Name: "plan_view",
			On: []string{
				"state.updated.plan",
				"state.updated.plan.>",
			},
			In:    HorizonRequest,
			Shape: ShapeKV,
		},
	}
}
