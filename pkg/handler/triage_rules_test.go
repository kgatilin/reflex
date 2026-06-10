package handler

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/projection"
)

// loadFixture reads a captured-from-prod JSON fixture under testdata/.
func loadFixture(t *testing.T, name string) json.RawMessage {
	t.Helper()
	p := filepath.Join("testdata", name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read fixture %s: %v", p, err)
	}
	return b
}

func TestClassifyArchai114IsStuck(t *testing.T) {
	comments := loadFixture(t, "archai_114_comments.json")
	timeline := loadFixture(t, "archai_114_timeline.json")
	// Same "now" Konstantin will run the verification at.
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	labelAge, kira, err := classifyInputs(comments, timeline, now, "kira-autonoma")
	if err != nil {
		t.Fatalf("classifyInputs: %v", err)
	}
	if kira != 0 {
		t.Fatalf("kira interactions = %d, want 0", kira)
	}
	if labelAge < 48*time.Hour {
		t.Fatalf("label_age = %v, want > 48h", labelAge)
	}
}

func TestClassifyArchai98IsHealthy(t *testing.T) {
	comments := loadFixture(t, "archai_98_comments.json")
	timeline := loadFixture(t, "archai_98_timeline.json")
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	_, kira, err := classifyInputs(comments, timeline, now, "kira-autonoma")
	if err != nil {
		t.Fatalf("classifyInputs: %v", err)
	}
	if kira == 0 {
		t.Fatal("expected at least 1 kira interaction (cross-referenced PR #113)")
	}
}

func TestTriageRulesEmitsTriageDecidedOnBothPaths(t *testing.T) {
	cfg := config.HandlerConfig{
		Name: "classify", Type: "triage_rules", On: projection.TypeGhQueryResult,
		Config: map[string]any{"now": "2026-06-07T12:00:00Z"},
	}
	sub, err := newTriageRules(cfg)
	if err != nil {
		t.Fatalf("newTriageRules: %v", err)
	}
	commentsRaw := loadFixture(t, "archai_114_comments.json")
	timelineRaw := loadFixture(t, "archai_114_timeline.json")
	log := []event.Event{
		{ID: "g1", Type: projection.TypeGhQueryResult, RequestID: "r",
			Payload: jsonRaw(map[string]any{"path": "comments", "json": commentsRaw})},
		{ID: "g2", Type: projection.TypeGhQueryResult, RequestID: "r",
			Payload: jsonRaw(map[string]any{"path": "timeline", "json": timelineRaw})},
	}
	ev := log[1]
	out, err := sub.React(context.Background(), ev, log)
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	if len(out) != 1 || out[0].Type != projection.TypeTriageDecided {
		t.Fatalf("got %+v", out)
	}
	var p struct {
		Classification   string `json:"classification"`
		Reason           string `json:"reason"`
		LabelAgeHours    int    `json:"label_age_hours"`
		KiraInteractions int    `json:"kira_interactions"`
	}
	if err := out[0].PayloadAs(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Classification != ClassStuck {
		t.Fatalf("classification = %s, want STUCK", p.Classification)
	}
	if !strings.Contains(p.Reason, "STUCK") || !strings.Contains(p.Reason, "kira=0") {
		t.Fatalf("reason = %q", p.Reason)
	}
}

func TestTriageRulesEmitsTriagePendingWhenOnlyOnePath(t *testing.T) {
	sub, _ := newTriageRules(config.HandlerConfig{
		Name: "classify", Type: "triage_rules", On: projection.TypeGhQueryResult,
		Config: map[string]any{"now": "2026-06-07T12:00:00Z"},
	})
	log := []event.Event{
		{ID: "g1", Type: projection.TypeGhQueryResult, RequestID: "r",
			Payload: jsonRaw(map[string]any{"path": "comments", "json": json.RawMessage(`[]`)})},
	}
	ev := log[0]
	out, err := sub.React(context.Background(), ev, log)
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	if len(out) != 1 || out[0].Type != projection.TypeTriagePending {
		t.Fatalf("got %+v", out)
	}
	if !out[0].Terminal {
		t.Fatal("TriagePending must be terminal to satisfy invariant")
	}
}

func TestTriageRulesIsIdempotent(t *testing.T) {
	sub, _ := newTriageRules(config.HandlerConfig{
		Name: "classify", Type: "triage_rules", On: projection.TypeGhQueryResult,
		Config: map[string]any{"now": "2026-06-07T12:00:00Z"},
	})
	commentsRaw := loadFixture(t, "archai_114_comments.json")
	timelineRaw := loadFixture(t, "archai_114_timeline.json")
	log := []event.Event{
		{ID: "g1", Type: projection.TypeGhQueryResult, RequestID: "r",
			Payload: jsonRaw(map[string]any{"path": "comments", "json": commentsRaw})},
		{ID: "g2", Type: projection.TypeGhQueryResult, RequestID: "r",
			Payload: jsonRaw(map[string]any{"path": "timeline", "json": timelineRaw})},
		{ID: "d1", Type: projection.TypeTriageDecided, RequestID: "r"},
	}
	out, err := sub.React(context.Background(), log[1], log)
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	// On second fire (TriageDecided already in log), the handler emits a
	// terminal TriagePending to satisfy the Phase 1 invariant — but never
	// a second TriageDecided.
	for _, e := range out {
		if e.Type == projection.TypeTriageDecided {
			t.Fatalf("second fire produced TriageDecided again: %+v", out)
		}
	}
}

func TestClassifySyntheticFreshAndHealthy(t *testing.T) {
	// Synthetic timeline: agent-ready label very recent, no kira interactions.
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	freshTimeline := json.RawMessage(`[
		{"event":"labeled","created_at":"2026-06-07T10:00:00Z","label":{"name":"agent-ready"},"actor":{"login":"kgatilin"}}
	]`)
	emptyComments := json.RawMessage(`[]`)
	labelAge, kira, err := classifyInputs(emptyComments, freshTimeline, now, "kira-autonoma")
	if err != nil {
		t.Fatalf("classifyInputs fresh: %v", err)
	}
	if kira != 0 {
		t.Fatalf("kira = %d, want 0", kira)
	}
	if labelAge >= 48*time.Hour {
		t.Fatalf("labelAge = %v, want < 48h", labelAge)
	}

	// Synthetic: kira commented → HEALTHY.
	healthyComments := json.RawMessage(`[{"user":{"login":"kira-autonoma"},"created_at":"2026-06-07T11:00:00Z"}]`)
	_, kira2, err := classifyInputs(healthyComments, freshTimeline, now, "kira-autonoma")
	if err != nil {
		t.Fatalf("classifyInputs healthy: %v", err)
	}
	if kira2 != 1 {
		t.Fatalf("kira = %d, want 1", kira2)
	}
}

func TestCrossReferencedKiraPRCounts(t *testing.T) {
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	timeline := json.RawMessage(`[
		{"event":"labeled","created_at":"2026-05-11T07:24:10Z","label":{"name":"agent-ready"},"actor":{"login":"kgatilin"}},
		{"event":"cross-referenced","created_at":"2026-05-11T06:41:38Z","actor":{"login":"kira-autonoma"},"source":{"issue":{"number":113,"user":{"login":"kira-autonoma"},"pull_request":{"url":"x"}}}}
	]`)
	_, kira, err := classifyInputs(json.RawMessage(`[]`), timeline, now, "kira-autonoma")
	if err != nil {
		t.Fatalf("classifyInputs: %v", err)
	}
	if kira != 1 {
		t.Fatalf("kira = %d, want 1", kira)
	}
}

func TestMentionedKiraDoesNotCount(t *testing.T) {
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	timeline := json.RawMessage(`[
		{"event":"labeled","created_at":"2026-05-30T13:53:17Z","label":{"name":"agent-ready"},"actor":{"login":"kgatilin"}},
		{"event":"mentioned","created_at":"2026-06-01T16:00:58Z","actor":{"login":"kira-autonoma"}},
		{"event":"subscribed","created_at":"2026-06-01T16:00:58Z","actor":{"login":"kira-autonoma"}}
	]`)
	_, kira, err := classifyInputs(json.RawMessage(`[]`), timeline, now, "kira-autonoma")
	if err != nil {
		t.Fatalf("classifyInputs: %v", err)
	}
	if kira != 0 {
		t.Fatalf("kira = %d, want 0 (mentioned/subscribed are false positives)", kira)
	}
}
