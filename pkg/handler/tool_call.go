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

// toolCallConfig binds a handler to a single named tool. Multiple tools are
// modelled as multiple handler entries in the YAML.
type toolCallConfig struct {
	Tool string `yaml:"tool"`
}

// builtin tools — extend by adding to the map. Each takes the raw `args`
// string from the ToolCallProposed payload and returns a textual result.
var builtinTools = map[string]func(args string) (string, error){
	"calc":   calcTool,
	"echo":   func(a string) (string, error) { return a, nil },
	"length": func(a string) (string, error) { return strconv.Itoa(len(a)), nil },
	"upper":  func(a string) (string, error) { return strings.ToUpper(a), nil },
}

func newToolCall(cfg config.HandlerConfig) (bus.Subscriber, error) {
	var tc toolCallConfig
	if err := decodeConfig(cfg.Config, &tc); err != nil {
		return nil, fmt.Errorf("tool_call %q: %w", cfg.Name, err)
	}
	if tc.Tool == "" {
		return nil, fmt.Errorf("tool_call %q: tool is required", cfg.Name)
	}
	if _, ok := builtinTools[tc.Tool]; !ok {
		return nil, fmt.Errorf("tool_call %q: unknown builtin tool %q", cfg.Name, tc.Tool)
	}

	bound := tc.Tool
	on := cfg.On
	return &genericSub{
		baseSub: baseSub{name: cfg.Name},
		on:      on,
		run: func(_ context.Context, ev event.Event, _ []event.Event) ([]event.Event, error) {
			var p struct {
				Tool string `json:"tool"`
				Args string `json:"args"`
			}
			if err := ev.PayloadAs(&p); err != nil {
				return nil, fmt.Errorf("tool_call %q: decode payload: %w", bound, err)
			}
			if p.Tool != bound {
				// Not addressed at us.
				return nil, nil
			}
			res, err := builtinTools[bound](p.Args)
			if err != nil {
				return nil, fmt.Errorf("tool %q: %w", bound, err)
			}
			payload, err := json.Marshal(map[string]string{"result": res})
			if err != nil {
				return nil, err
			}
			return []event.Event{{
				Type:    projection.TypeToolResultObserved,
				Payload: payload,
			}}, nil
		},
	}, nil
}

// calcTool evaluates a minimal `a+b`, `a-b`, `a*b`, `a/b` expression where a
// and b are integers. It scans the input for the first such expression so
// "what is 2+2" works as well as "2+2". Intentionally tiny — reflex wants a
// tool that works without pulling in a parser dependency.
func calcTool(args string) (string, error) {
	expr := extractMathExpr(args)
	if expr == "" {
		return "", fmt.Errorf("calc: cannot find expression in %q", args)
	}
	for _, op := range []string{"+", "-", "*", "/"} {
		i := strings.Index(expr, op)
		if i <= 0 {
			continue
		}
		left, err := strconv.Atoi(expr[:i])
		if err != nil {
			continue
		}
		right, err := strconv.Atoi(expr[i+1:])
		if err != nil {
			continue
		}
		switch op {
		case "+":
			return strconv.Itoa(left + right), nil
		case "-":
			return strconv.Itoa(left - right), nil
		case "*":
			return strconv.Itoa(left * right), nil
		case "/":
			if right == 0 {
				return "", fmt.Errorf("calc: division by zero")
			}
			return strconv.Itoa(left / right), nil
		}
	}
	return "", fmt.Errorf("calc: cannot parse %q (extracted %q)", args, expr)
}

// extractMathExpr scans s for a contiguous run of digits and one of +-*/ and
// returns it (whitespace stripped). The first such run wins.
func extractMathExpr(s string) string {
	s = strings.ReplaceAll(s, " ", "")
	isExprByte := func(c byte) bool {
		return (c >= '0' && c <= '9') || c == '+' || c == '-' || c == '*' || c == '/'
	}
	start := -1
	for i := 0; i < len(s); i++ {
		if isExprByte(s[i]) {
			if start == -1 {
				start = i
			}
			continue
		}
		if start != -1 {
			candidate := s[start:i]
			if hasOp(candidate) {
				return candidate
			}
			start = -1
		}
	}
	if start != -1 {
		candidate := s[start:]
		if hasOp(candidate) {
			return candidate
		}
	}
	return ""
}

func hasOp(s string) bool {
	return strings.ContainsAny(s, "+-*/")
}
