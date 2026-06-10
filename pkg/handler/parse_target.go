package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/projection"
)

// parseTargetConfig is the YAML knobs block. Empty is fine; defaults take over.
type parseTargetConfig struct {
	// DefaultOwner is used when the input is "repo#number" rather than
	// "owner/repo#number". Defaults to "kgatilin".
	DefaultOwner string `yaml:"default_owner"`
}

// newParseTarget extracts an {owner, repo, number} triple from a user message
// payload like "archai#114" or "kgatilin/archai#114" and emits TargetParsed.
// Unparseable input emits ParseFailed (terminal).
//
// The handler reads the trigger event's "payload" string from its payload
// JSON, which is the RequestReceived convention in reflex.
func newParseTarget(cfg config.HandlerConfig) (bus.Subscriber, error) {
	var pc parseTargetConfig
	if err := decodeConfig(cfg.Config, &pc); err != nil {
		return nil, fmt.Errorf("parse_target %q: %w", cfg.Name, err)
	}
	if pc.DefaultOwner == "" {
		pc.DefaultOwner = "kgatilin"
	}
	defaultOwner := pc.DefaultOwner
	name := cfg.Name
	on := cfg.On

	return &genericSub{
		baseSub: baseSub{name: name},
		on:      on,
		run: func(_ context.Context, ev event.Event, _ []event.Event) ([]event.Event, error) {
			var p struct {
				Payload string `json:"payload"`
			}
			if err := ev.PayloadAs(&p); err != nil {
				return nil, fmt.Errorf("parse_target %q: decode payload: %w", name, err)
			}
			owner, repo, number, perr := parseTargetString(p.Payload, defaultOwner)
			if perr != nil {
				failPayload, _ := json.Marshal(map[string]string{
					"input": p.Payload,
					"error": perr.Error(),
				})
				return []event.Event{{
					Type:     projection.TypeParseFailed,
					Payload:  failPayload,
					Terminal: true,
				}}, nil
			}
			okPayload, err := json.Marshal(map[string]any{
				"owner":  owner,
				"repo":   repo,
				"number": number,
			})
			if err != nil {
				return nil, err
			}
			return []event.Event{{
				Type:    projection.TypeTargetParsed,
				Payload: okPayload,
			}}, nil
		},
	}, nil
}

// parseTargetString accepts "owner/repo#number" or "repo#number" (the latter
// uses defaultOwner). Leading/trailing whitespace is ignored. Validation
// is deliberately strict: empty fields or a non-numeric issue number fail.
func parseTargetString(in, defaultOwner string) (owner, repo string, number int, err error) {
	s := strings.TrimSpace(in)
	if s == "" {
		return "", "", 0, fmt.Errorf("empty input")
	}
	hash := strings.Index(s, "#")
	if hash < 0 {
		return "", "", 0, fmt.Errorf("missing '#': want owner/repo#N or repo#N, got %q", in)
	}
	left := s[:hash]
	right := s[hash+1:]
	if left == "" {
		return "", "", 0, fmt.Errorf("missing repo before '#'")
	}
	if right == "" {
		return "", "", 0, fmt.Errorf("missing issue number after '#'")
	}
	n, convErr := strconv.Atoi(right)
	if convErr != nil {
		return "", "", 0, fmt.Errorf("issue number %q is not an integer: %w", right, convErr)
	}
	if slash := strings.Index(left, "/"); slash >= 0 {
		owner = strings.TrimSpace(left[:slash])
		repo = strings.TrimSpace(left[slash+1:])
		if owner == "" || repo == "" {
			return "", "", 0, fmt.Errorf("malformed owner/repo in %q", left)
		}
	} else {
		owner = defaultOwner
		repo = strings.TrimSpace(left)
	}
	return owner, repo, n, nil
}
