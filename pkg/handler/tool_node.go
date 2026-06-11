package handler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
)

// tool_node is the subject-typed sibling of tool_call. Where tool_call routes
// on a payload field (ToolCallProposed{tool}), tool_node routes on the event
// TYPE: it subscribes to tool.<tool>.call and emits tool.<tool>.result on
// success or tool.<tool>.failed on a tool error. This is the shape the domain
// model wants — routing lives in the type, never the payload — and it lets a
// tool be a first-class node in the static graph (its result/failure types
// are declared edges).
//
// It reuses the same builtinTools map as tool_call so there is exactly one
// implementation of each tool.
type toolNodeConfig struct {
	// Tool is the builtin tool name (calc, echo, length, upper).
	Tool string `json:"tool"`
	// Emit overrides the success event type; defaults to tool.<tool>.result.
	Emit string `json:"emit"`
	// Fail overrides the failure event type; defaults to tool.<tool>.failed.
	Fail string `json:"fail"`
}

func newToolNode(cfg config.HandlerConfig) (bus.Subscriber, error) {
	var tc toolNodeConfig
	if err := decodeConfig(cfg.Config, &tc); err != nil {
		return nil, fmt.Errorf("tool_node %q: %w", cfg.Name, err)
	}
	if tc.Tool == "" {
		return nil, fmt.Errorf("tool_node %q: tool is required", cfg.Name)
	}
	fn, ok := builtinTools[tc.Tool]
	if !ok {
		return nil, fmt.Errorf("tool_node %q: unknown builtin tool %q", cfg.Name, tc.Tool)
	}
	if tc.Emit == "" {
		tc.Emit = "tool." + tc.Tool + ".result"
	}
	if tc.Fail == "" {
		tc.Fail = "tool." + tc.Tool + ".failed"
	}

	on := cfg.On
	name := cfg.Name
	emit := tc.Emit
	fail := tc.Fail
	return &genericSub{
		baseSub: baseSub{name: name},
		on:      on,
		run: func(_ context.Context, ev event.Event, _ []event.Event) ([]event.Event, error) {
			var p struct {
				Args string `json:"args"`
			}
			if err := ev.PayloadAs(&p); err != nil {
				return nil, fmt.Errorf("tool_node %q: decode payload: %w", name, err)
			}
			res, err := fn(p.Args)
			if err != nil {
				// Tool errors are events, not Go errors — returning a Go
				// error would abort the drain. Emit a non-terminal
				// tool.<tool>.failed the topology can react to (retry,
				// fallback) or visibly orphan.
				failPayload, mErr := json.Marshal(map[string]string{"error": err.Error()})
				if mErr != nil {
					return nil, mErr
				}
				return []event.Event{{
					Type:    fail,
					Payload: failPayload,
				}}, nil
			}
			okPayload, err := json.Marshal(map[string]string{"result": res})
			if err != nil {
				return nil, err
			}
			return []event.Event{{
				Type:    emit,
				Payload: okPayload,
			}}, nil
		},
	}, nil
}
