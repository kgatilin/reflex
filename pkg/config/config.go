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
// list of declared subscribers. Events is the optional Phase 1.6 section
// that declares known event types and their CLI bindings. Permissions is
// the optional Phase 4c top-level block that seeds boot-time grants into
// the bus permission table before any handler registers.
type File struct {
	Settings    Settings           `yaml:"settings"`
	Handlers    []HandlerConfig    `yaml:"handlers"`
	Events      []EventConfig      `yaml:"events"`
	Permissions []PermissionConfig `yaml:"permissions"`
}

// PermissionConfig is one top-level permissions entry. The loader emits
// one PermissionGranted control-plane event per entry at boot, BEFORE
// any HandlerRegistered events fire — so the permission table is hot
// when the bus starts gating runtime mutations.
//
// Field names mirror the four axes of the permission model:
//
//	mutate:      list of scope globs the principal may publish
//	             control-plane events targeting
//	read:        list of scope globs the principal may receive from
//	forbidden:   explicit deny list; overrides mutate / read
//	meta_grant:  list of scope globs the principal may delegate further
//	             (i.e. publish PermissionGranted events for)
//
// The YAML key for meta-grant is `meta.grant` (matches the docs); the
// loader rewrites it to MetaGrant on the Go side.
type PermissionConfig struct {
	Principal string       `yaml:"principal"`
	Grants    GrantsConfig `yaml:"grants"`
}

// GrantsConfig carries the four-axis permission grants for one principal.
type GrantsConfig struct {
	Mutate    []string `yaml:"mutate"`
	Read      []string `yaml:"read"`
	Forbidden []string `yaml:"forbidden"`
	// MetaGrant maps to the YAML key `meta.grant` (dot in the literal
	// string is preserved by yaml.v3 when quoted in the field tag).
	MetaGrant []string `yaml:"meta.grant"`
}

// EventConfig declares a known event type so the CLI can emit it directly
// (`reflex emit <type>` or `reflex invoke <command>`), and so the analyzer
// can validate trace events against a schema.
//
// Args is an open map: keys are payload field names, values are type hints
// (e.g. "string", "int"). The current CLI only consumes Args names when
// binding positional flags; type validation is left to handlers.
//
// CLI is the optional binding: it tells the CLI how to surface this event
// as a friendly subcommand. Command="invoke triage" means
// `reflex invoke triage <args>` emits this event. Wait names the
// wait-predicate the CLI applies after emission (drain |
// request_id_terminal | projection.has=<key>).
type EventConfig struct {
	Name string         `yaml:"name"`
	Args map[string]any `yaml:"args"`
	CLI  *EventCLI      `yaml:"cli,omitempty"`
}

// EventCLI is the CLI binding for an EventConfig.
type EventCLI struct {
	Command string `yaml:"command"`
	Wait    string `yaml:"wait"`
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
	Name        string         `yaml:"name"`
	Type        string         `yaml:"type"`
	On          string         `yaml:"on"`
	Emits       []string       `yaml:"emits"`
	Config      map[string]any `yaml:"config"`
	Loop        *LoopConfig    `yaml:"loop,omitempty"`
	Scope       string         `yaml:"scope,omitempty"`
	Permissions *GrantsConfig  `yaml:"permissions,omitempty"`
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
