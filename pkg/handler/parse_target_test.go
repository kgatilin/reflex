package handler

import (
	"context"
	"testing"

	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/projection"
)

func TestParseTargetHappyPaths(t *testing.T) {
	cases := []struct {
		name       string
		input      string
		defOwner   string
		wantOwner  string
		wantRepo   string
		wantNumber int
	}{
		{"short form", "archai#114", "kgatilin", "kgatilin", "archai", 114},
		{"qualified", "mshogin/archlint#42", "kgatilin", "mshogin", "archlint", 42},
		{"override default", "promptlint#7", "mikeshogin", "mikeshogin", "promptlint", 7},
		{"whitespace trimmed", "  archai#99 ", "kgatilin", "kgatilin", "archai", 99},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo, n, err := parseTargetString(tc.input, tc.defOwner)
			if err != nil {
				t.Fatalf("parseTargetString(%q): %v", tc.input, err)
			}
			if owner != tc.wantOwner || repo != tc.wantRepo || n != tc.wantNumber {
				t.Fatalf("got owner=%s repo=%s n=%d want %s/%s#%d",
					owner, repo, n, tc.wantOwner, tc.wantRepo, tc.wantNumber)
			}
		})
	}
}

func TestParseTargetSadPaths(t *testing.T) {
	bad := []string{"", "no-hash-here", "archai#", "#114", "archai#abc", "/repo#1", "owner/#1"}
	for _, in := range bad {
		if _, _, _, err := parseTargetString(in, "kgatilin"); err == nil {
			t.Errorf("expected error for %q, got nil", in)
		}
	}
}

func TestParseTargetHandlerEmitsTargetParsed(t *testing.T) {
	sub, err := newParseTarget(config.HandlerConfig{
		Name: "parse",
		Type: "parse_target",
		On:   projection.TypeRequestReceived,
	})
	if err != nil {
		t.Fatalf("newParseTarget: %v", err)
	}
	ev := event.Event{
		Type:    projection.TypeRequestReceived,
		Payload: jsonRaw(map[string]string{"payload": "archai#114"}),
	}
	out, err := sub.React(context.Background(), ev, nil)
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	if len(out) != 1 || out[0].Type != projection.TypeTargetParsed {
		t.Fatalf("got %+v", out)
	}
	if out[0].Terminal {
		t.Fatal("TargetParsed must not be terminal")
	}
	var got struct {
		Owner  string `json:"owner"`
		Repo   string `json:"repo"`
		Number int    `json:"number"`
	}
	_ = out[0].PayloadAs(&got)
	if got.Owner != "kgatilin" || got.Repo != "archai" || got.Number != 114 {
		t.Fatalf("payload = %+v", got)
	}
}

func TestParseTargetHandlerEmitsParseFailedTerminal(t *testing.T) {
	sub, _ := newParseTarget(config.HandlerConfig{
		Name: "parse", Type: "parse_target", On: projection.TypeRequestReceived,
	})
	ev := event.Event{
		Type:    projection.TypeRequestReceived,
		Payload: jsonRaw(map[string]string{"payload": "not a target"}),
	}
	out, err := sub.React(context.Background(), ev, nil)
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	if len(out) != 1 || out[0].Type != projection.TypeParseFailed {
		t.Fatalf("got %+v", out)
	}
	if !out[0].Terminal {
		t.Fatal("ParseFailed must be terminal")
	}
}
