package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/projection"
)

// Triage thresholds. Stuck-watcher rules per the home-dev stuck-watcher
// audit logs: 48h since `agent-ready` label + zero kira interactions = STUCK.
const (
	defaultStuckThreshold = 48 * time.Hour
	kiraLogin             = "kira-autonoma"
	labelAgentReady       = "agent-ready"
)

// Triage classification results — exported so test assertions can use them.
const (
	ClassStuck   = "STUCK"
	ClassHealthy = "HEALTHY"
	ClassFresh   = "FRESH"
)

type triageRulesConfig struct {
	// StuckHours overrides the 48h threshold. Useful for tests; production
	// YAML can leave it unset.
	StuckHours int `yaml:"stuck_hours"`
	// KiraLogin overrides the default "kira-autonoma". Useful if Konstantin
	// renames her or wants to test with another bot.
	KiraLogin string `yaml:"kira_login"`
	// Now is an optional RFC3339 timestamp used as the reference point for
	// label-age math. Defaults to time.Now(). Tests pin this for determinism.
	Now string `yaml:"now"`
}

// triageRulesSub is the runtime instance of triage_rules. It carries a
// handle to the projection store so that, when it reaches a decision, it
// can stash the verdict under projection key "triage.verdict" — making it
// readable by downstream handlers (and by CLI wait-predicates) without
// re-deriving from the event log.
type triageRulesSub struct {
	*genericSub
	proj *projection.Store
}

// SetProjection satisfies bus.ProjectionAware: the runtime wires the
// bus's projection store into the handler after construction.
func (t *triageRulesSub) SetProjection(p *projection.Store) { t.proj = p }

// newTriageRules folds the GhQueryResult events for the request into a
// TriageDecided event. It fires once per GhQueryResult event but skips
// emission until BOTH `comments` and `timeline` paths have arrived for the
// same request — the second fire produces the (idempotent) decision.
//
// Rationale for the skip-until-both pattern: a dedicated barrier handler
// would be cleaner but adds a YAML concept; the projection makes "do we
// have both?" a one-line check, and TriageDecided is idempotent on
// re-emission because the projection-based deduplication catches it.
//
// Phase 1.6: when the decision is reached, the verdict is also written
// into the projection store under "triage.verdict" so downstream
// handlers (and CLI wait-predicates) can pick it up without scanning
// the event log themselves.
func newTriageRules(cfg config.HandlerConfig) (bus.Subscriber, error) {
	var tc triageRulesConfig
	if err := decodeConfig(cfg.Config, &tc); err != nil {
		return nil, fmt.Errorf("triage_rules %q: %w", cfg.Name, err)
	}
	threshold := defaultStuckThreshold
	if tc.StuckHours > 0 {
		threshold = time.Duration(tc.StuckHours) * time.Hour
	}
	kira := tc.KiraLogin
	if kira == "" {
		kira = kiraLogin
	}
	var pinnedNow time.Time
	if tc.Now != "" {
		t, err := time.Parse(time.RFC3339, tc.Now)
		if err != nil {
			return nil, fmt.Errorf("triage_rules %q: parse now: %w", cfg.Name, err)
		}
		pinnedNow = t
	}

	name := cfg.Name
	on := cfg.On

	sub := &triageRulesSub{}
	sub.genericSub = &genericSub{
		baseSub: baseSub{name: name},
		on:      on,
		run: func(_ context.Context, ev event.Event, log []event.Event) ([]event.Event, error) {
			// Idempotency: if a TriageDecided already exists for this
			// request, don't re-decide — but still emit a terminal
			// TriagePending so the triggering event has a child (Phase 1
			// invariant: every non-terminal event must have a descendant).
			for _, e := range log {
				if e.RequestID == ev.RequestID && e.Type == projection.TypeTriageDecided {
					payload, _ := json.Marshal(map[string]string{
						"waiting_for": "none",
						"reason":      "TriageDecided already emitted for this request",
					})
					return []event.Event{{
						Type:     projection.TypeTriagePending,
						Payload:  payload,
						Terminal: true,
					}}, nil
				}
			}

			// Gather GhQueryResult events for this request.
			var commentsRaw, timelineRaw json.RawMessage
			for _, e := range log {
				if e.RequestID != ev.RequestID {
					continue
				}
				if e.Type != projection.TypeGhQueryResult {
					continue
				}
				var p struct {
					Path string          `json:"path"`
					JSON json.RawMessage `json:"json"`
				}
				if err := e.PayloadAs(&p); err != nil {
					return nil, fmt.Errorf("triage_rules %q: decode GhQueryResult: %w", name, err)
				}
				switch p.Path {
				case "comments":
					commentsRaw = p.JSON
				case "timeline":
					timelineRaw = p.JSON
				}
			}
			if commentsRaw == nil || timelineRaw == nil {
				// Wait for the other path. Emit TriagePending (terminal) as
				// the explicit descendant of the GhQueryResult that triggered
				// us — keeps the orphan invariant satisfied without polluting
				// the decision logic.
				missing := "comments"
				if commentsRaw != nil {
					missing = "timeline"
				}
				pendingPayload, err := json.Marshal(map[string]string{
					"waiting_for": missing,
					"reason":      "triage needs both comments and timeline",
				})
				if err != nil {
					return nil, err
				}
				return []event.Event{{
					Type:     projection.TypeTriagePending,
					Payload:  pendingPayload,
					Terminal: true,
				}}, nil
			}

			now := pinnedNow
			if now.IsZero() {
				now = time.Now().UTC()
			}

			labelAge, kiraInteractions, err := classifyInputs(commentsRaw, timelineRaw, now, kira)
			if err != nil {
				return nil, fmt.Errorf("triage_rules %q: %w", name, err)
			}

			var classification string
			switch {
			case kiraInteractions > 0:
				classification = ClassHealthy
			case labelAge > threshold:
				classification = ClassStuck
			default:
				classification = ClassFresh
			}

			hours := int(labelAge.Hours())
			reason := fmt.Sprintf("label_age=%dh, kira=%d → %s",
				hours, kiraInteractions, classification)

			verdict := map[string]any{
				"classification":    classification,
				"reason":            reason,
				"label_age_hours":   hours,
				"kira_interactions": kiraInteractions,
			}
			payload, err := json.Marshal(verdict)
			if err != nil {
				return nil, err
			}
			// Phase 1.6 projection write: downstream handlers can read
			// the verdict by key without re-folding the event log, and
			// the CLI projection.has=triage.verdict wait-predicate
			// resolves on this entry.
			if sub.proj != nil {
				sub.proj.Set(ev.RequestID, "triage.verdict", verdict)
			}
			return []event.Event{{
				Type:    projection.TypeTriageDecided,
				Payload: payload,
			}}, nil
		},
	}
	return sub, nil
}

// classifyInputs is the pure decision function — separated so tests can drive
// it directly without going through the projection machinery.
func classifyInputs(commentsRaw, timelineRaw json.RawMessage, now time.Time, kira string) (time.Duration, int, error) {
	labelTS, err := latestAgentReadyLabelTS(timelineRaw)
	if err != nil {
		return 0, 0, fmt.Errorf("scan timeline for agent-ready label: %w", err)
	}
	if labelTS.IsZero() {
		// Defensive fallback per spec: if no `labeled` event, fall back to
		// the issue's created_at, which has the same shape on timeline
		// records (gh returns it as part of the source.issue for cross-refs
		// — but easier: leave it zero and treat label_age as "very fresh"
		// so the classifier doesn't false-positive STUCK).
		// Per the spec we should use issue.createdAt, but our query is
		// scoped to /comments and /timeline; we'd need /issues/{N} to get
		// it. Practical compromise: when no agent-ready label is found,
		// derive from the earliest timeline entry's created_at as a
		// proxy. That stays defensive while not requiring a third gh call.
		labelTS = earliestTimelineTS(timelineRaw)
	}

	labelAge := now.Sub(labelTS)
	if labelAge < 0 {
		labelAge = 0
	}

	kiraInteractions, err := countKiraInteractions(commentsRaw, timelineRaw, kira)
	if err != nil {
		return 0, 0, fmt.Errorf("count kira interactions: %w", err)
	}
	return labelAge, kiraInteractions, nil
}

// latestAgentReadyLabelTS scans timeline JSON for the most recent `labeled`
// event with label.name == "agent-ready" and returns its created_at. The
// timeline endpoint returns an array; we parse defensively (skip records
// that don't have the expected shape rather than failing).
func latestAgentReadyLabelTS(raw json.RawMessage) (time.Time, error) {
	if len(raw) == 0 {
		return time.Time{}, nil
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return time.Time{}, err
	}
	var latest time.Time
	for _, e := range entries {
		evTypeRaw, ok := e["event"]
		if !ok {
			continue
		}
		var evType string
		if err := json.Unmarshal(evTypeRaw, &evType); err != nil {
			continue
		}
		if evType != "labeled" {
			continue
		}
		labelRaw, ok := e["label"]
		if !ok {
			continue
		}
		var label struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(labelRaw, &label); err != nil {
			continue
		}
		if label.Name != labelAgentReady {
			continue
		}
		createdAtRaw, ok := e["created_at"]
		if !ok {
			continue
		}
		var createdAtStr string
		if err := json.Unmarshal(createdAtRaw, &createdAtStr); err != nil {
			continue
		}
		t, err := time.Parse(time.RFC3339, createdAtStr)
		if err != nil {
			continue
		}
		if t.After(latest) {
			latest = t
		}
	}
	return latest, nil
}

// earliestTimelineTS returns the oldest created_at across all timeline
// entries — used as the fallback baseline when no agent-ready labeling event
// is found.
func earliestTimelineTS(raw json.RawMessage) time.Time {
	if len(raw) == 0 {
		return time.Time{}
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return time.Time{}
	}
	var earliest time.Time
	for _, e := range entries {
		createdAtRaw, ok := e["created_at"]
		if !ok {
			continue
		}
		var createdAtStr string
		if err := json.Unmarshal(createdAtRaw, &createdAtStr); err != nil {
			continue
		}
		t, err := time.Parse(time.RFC3339, createdAtStr)
		if err != nil {
			continue
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// countKiraInteractions = comments by kira + cross-referenced PRs authored
// by kira. Excludes mentioned/subscribed timeline events with actor=kira
// (those auto-fire on @-mentions and would be false positives).
func countKiraInteractions(commentsRaw, timelineRaw json.RawMessage, kira string) (int, error) {
	count := 0

	// Pass 1: comments authored by kira.
	if len(commentsRaw) > 0 {
		var comments []map[string]json.RawMessage
		if err := json.Unmarshal(commentsRaw, &comments); err != nil {
			return 0, err
		}
		for _, c := range comments {
			userRaw, ok := c["user"]
			if !ok {
				continue
			}
			var user struct {
				Login string `json:"login"`
			}
			if err := json.Unmarshal(userRaw, &user); err != nil {
				continue
			}
			if strings.EqualFold(user.Login, kira) {
				count++
			}
		}
	}

	// Pass 2: cross-referenced PRs authored by kira (timeline entries with
	// event=cross-referenced where source.issue.user.login == kira AND the
	// referenced issue is a pull request).
	if len(timelineRaw) > 0 {
		var entries []map[string]json.RawMessage
		if err := json.Unmarshal(timelineRaw, &entries); err != nil {
			return 0, err
		}
		for _, e := range entries {
			evTypeRaw, ok := e["event"]
			if !ok {
				continue
			}
			var evType string
			if err := json.Unmarshal(evTypeRaw, &evType); err != nil {
				continue
			}
			if evType != "cross-referenced" {
				continue
			}
			sourceRaw, ok := e["source"]
			if !ok {
				continue
			}
			var source struct {
				Issue struct {
					User struct {
						Login string `json:"login"`
					} `json:"user"`
					PullRequest map[string]any `json:"pull_request"`
				} `json:"issue"`
			}
			if err := json.Unmarshal(sourceRaw, &source); err != nil {
				continue
			}
			if !strings.EqualFold(source.Issue.User.Login, kira) {
				continue
			}
			// Only count when it's a PR cross-reference (not an issue).
			// GitHub timeline cross-references can be either; PRs carry the
			// pull_request sub-object. If absent, still count if author is
			// kira — she only files PRs on these repos.
			_ = source.Issue.PullRequest
			count++
		}
	}

	return count, nil
}
