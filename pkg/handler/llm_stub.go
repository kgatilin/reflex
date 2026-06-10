package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/projection"
)

// llmStubRule is one deterministic branch in the stub LLM. The first rule
// whose Match substring is present in the trigger (user message or last tool
// result) wins; on tie, declaration order decides.
type llmStubRule struct {
	Match string `yaml:"match" mapstructure:"match"`
	// Action is one of: "tool_call", "reply", "reply_and_handle".
	Action string `yaml:"action" mapstructure:"action"`
	Tool   string `yaml:"tool" mapstructure:"tool"`
	Args   string `yaml:"args" mapstructure:"args"`
	Reply  string `yaml:"reply" mapstructure:"reply"`
}

type llmStubConfig struct {
	// TriggerOn declares which event types invoke the stub. Defaults to
	// just RequestReceived. Configure ["RequestReceived",
	// "ToolResultObserved"] to make the stub react after every tool call.
	TriggerOn []string `yaml:"trigger_on"`
	// Fallback is what happens when no rule matches. Action values are the
	// same as for rules.
	Fallback llmStubRule   `yaml:"fallback"`
	Rules    []llmStubRule `yaml:"rules"`
}

// newLLMStub builds a deterministic stub-LLM subscriber. The stub matches on
// substrings — see the YAML rules in examples/ for what wins. Real Anthropic
// integration is intentionally out of scope; swap this handler for one that
// calls the SDK when you want a live model.
func newLLMStub(cfg config.HandlerConfig) (bus.Subscriber, error) {
	var lc llmStubConfig
	if err := decodeConfig(cfg.Config, &lc); err != nil {
		return nil, fmt.Errorf("llm_stub %q: %w", cfg.Name, err)
	}
	if len(lc.TriggerOn) == 0 {
		lc.TriggerOn = []string{cfg.On}
	}
	triggers := map[string]bool{}
	for _, t := range lc.TriggerOn {
		triggers[t] = true
	}

	name := cfg.Name
	return &llmStub{
		baseSub: baseSub{name: name},
		on:      cfg.On,
		trigger: triggers,
		rules:   lc.Rules,
		fb:      lc.Fallback,
	}, nil
}

type llmStub struct {
	baseSub
	on      string
	trigger map[string]bool
	rules   []llmStubRule
	fb      llmStubRule
}

func (l *llmStub) Match(ev event.Event) bool {
	return l.trigger[ev.Type]
}

func (l *llmStub) React(_ context.Context, ev event.Event, log []event.Event) ([]event.Event, error) {
	state := projection.SessionProjection(log, ev.RequestID)

	// The substring we match on depends on what triggered us.
	trigger := state.UserMessage
	if ev.Type == projection.TypeToolResultObserved {
		if r, ok := state.LastToolResult(); ok {
			trigger = r.Result
		}
	}

	chosen := l.fb
	for _, rule := range l.rules {
		if rule.Match == "" {
			continue
		}
		if strings.Contains(strings.ToLower(trigger), strings.ToLower(rule.Match)) {
			chosen = rule
			break
		}
	}

	return materialise(chosen, state, trigger)
}

// materialise turns a chosen rule into emitted events.
func materialise(rule llmStubRule, state projection.SessionState, trigger string) ([]event.Event, error) {
	var out []event.Event
	switch rule.Action {
	case "tool_call":
		if rule.Tool == "" {
			return nil, fmt.Errorf("llm_stub: rule action=tool_call requires tool")
		}
		args := rule.Args
		if args == "" {
			args = trigger
		}
		args = renderReply(args, state)
		payload, err := json.Marshal(map[string]string{
			"tool": rule.Tool,
			"args": args,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, event.Event{
			Type:    projection.TypeToolCallProposed,
			Payload: payload,
		})
	case "reply":
		text := renderReply(rule.Reply, state)
		payload, err := json.Marshal(map[string]string{"text": text})
		if err != nil {
			return nil, err
		}
		out = append(out, event.Event{
			Type:    projection.TypeAssistantMessageProposed,
			Payload: payload,
		})
	case "reply_and_handle":
		text := renderReply(rule.Reply, state)
		payload, err := json.Marshal(map[string]string{"text": text})
		if err != nil {
			return nil, err
		}
		// The assistant message is the *visible* leaf — the printer reads
		// it but emits nothing of its own. Mark it terminal so the orphan
		// watcher doesn't complain that AMP has no descendants.
		out = append(out, event.Event{
			Type:     projection.TypeAssistantMessageProposed,
			Payload:  payload,
			Terminal: true,
		})
		out = append(out, event.Event{
			Type:     projection.TypeRequestHandled,
			Terminal: true,
		})
	case "", "none":
		// Deliberate no-op — used by the stall example so nothing fires.
	default:
		return nil, fmt.Errorf("llm_stub: unknown action %q", rule.Action)
	}
	return out, nil
}

// renderReply does minimal templating: `{last_tool_result}` is replaced
// with the most recent tool result. Anything fancier belongs in a real LLM.
func renderReply(template string, state projection.SessionState) string {
	if template == "" {
		return ""
	}
	res := template
	if r, ok := state.LastToolResult(); ok {
		res = strings.ReplaceAll(res, "{last_tool_result}", r.Result)
	} else {
		res = strings.ReplaceAll(res, "{last_tool_result}", "")
	}
	res = strings.ReplaceAll(res, "{user_message}", state.UserMessage)
	return res
}
