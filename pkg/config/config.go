// Package config loads reflex handler definitions from YAML.
//
// A reflex config is a flat list of handlers. Each handler declares:
//   - name:    a free-form label used in the trace
//   - type:    one of the registered handler types (see pkg/handler)
//   - on:      the event type it subscribes to
//   - emits:   the event types it may emit (informational; checked for unknown
//     event names so typos in the YAML fail fast)
//   - config:  type-specific parameters (left opaque here, parsed by the
//     handler implementation)
//
// The package is intentionally validation-only. Wiring handlers into the bus
// happens in pkg/handler.
package config

import (
	"errors"
	"fmt"
	"os"
	"sort"

	"gopkg.in/yaml.v3"
)

// File is the top-level YAML document. Settings is optional; Handlers is the
// list of declared subscribers.
type File struct {
	Settings Settings        `yaml:"settings"`
	Handlers []HandlerConfig `yaml:"handlers"`
}

// Settings carries optional run-level knobs.
type Settings struct {
	MaxSteps int `yaml:"max_steps"`
}

// HandlerConfig is one handler entry.
//
// Config is a free-form map that handler implementations parse on their own
// terms. Keeping it opaque here means new handler types can be added without
// touching this package.
//
// Loop is the Phase 1.5 declaration that this handler closes a cycle in the
// handler graph. When present, MaxIterations becomes the edge weight on every
// outgoing edge of this handler that participates in a cycle, and the
// dispatcher refuses to fire this handler more than MaxIterations times per
// request_id (emitting LoopExhausted{loop_name, request_id} instead). An
// optional Name lets multiple disjoint loops coexist in the same config.
type HandlerConfig struct {
	Name   string         `yaml:"name"`
	Type   string         `yaml:"type"`
	On     string         `yaml:"on"`
	Emits  []string       `yaml:"emits"`
	Config map[string]any `yaml:"config"`
	Loop   *LoopConfig    `yaml:"loop,omitempty"`
}

// LoopConfig declares this handler as a cycle-closing node with a hard cap.
type LoopConfig struct {
	// MaxIterations is the per-request cap on this handler's fire count.
	// Must be > 0; the validator rejects 0 / negative values.
	MaxIterations int `yaml:"max_iterations"`
	// Name is an optional label for the loop, useful when more than one
	// loop exists in the same config. Defaults to the handler's own Name
	// when omitted.
	Name string `yaml:"name"`
}

// ErrUnknownHandlerType is reported when a handler type is not registered.
var ErrUnknownHandlerType = errors.New("config: unknown handler type")

// Load parses path as YAML and runs Validate against the supplied set of
// known handler types.
func Load(path string, knownTypes map[string]bool) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	return Parse(data, knownTypes)
}

// Parse decodes data as YAML and validates it against knownTypes.
func Parse(data []byte, knownTypes map[string]bool) (*File, error) {
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("config: yaml: %w", err)
	}
	if err := f.Validate(knownTypes); err != nil {
		return nil, err
	}
	return &f, nil
}

// Validate checks that every handler has a name, a known type, and a
// non-empty `on` field. Duplicate names are rejected so the trace is
// unambiguous.
func (f *File) Validate(knownTypes map[string]bool) error {
	if len(f.Handlers) == 0 {
		return errors.New("config: at least one handler is required")
	}
	seen := map[string]bool{}
	for i, h := range f.Handlers {
		if h.Name == "" {
			return fmt.Errorf("config: handler[%d]: name is required", i)
		}
		if seen[h.Name] {
			return fmt.Errorf("config: handler %q: duplicate name", h.Name)
		}
		seen[h.Name] = true
		if h.Type == "" {
			return fmt.Errorf("config: handler %q: type is required", h.Name)
		}
		if knownTypes != nil && !knownTypes[h.Type] {
			known := make([]string, 0, len(knownTypes))
			for k := range knownTypes {
				known = append(known, k)
			}
			sort.Strings(known)
			return fmt.Errorf("%w: %q (handler %q); known: %v",
				ErrUnknownHandlerType, h.Type, h.Name, known)
		}
		if h.On == "" {
			return fmt.Errorf("config: handler %q: on is required", h.Name)
		}
		if h.Loop != nil {
			if h.Loop.MaxIterations <= 0 {
				return fmt.Errorf("config: handler %q: loop.max_iterations must be > 0", h.Name)
			}
		}
	}
	return nil
}
