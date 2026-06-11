package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
)

// audit is the Phase 4b reference audit handler. It subscribes to the
// control-plane meta-events (HandlerRegistered, Subscribed, Unsubscribed,
// HandlerDeregistered) and writes each one to a JSONL sink. Demonstrates
// that audit lives inside the bus as an ordinary handler — no privileged
// side-channel.
//
// Configurable via `config.sink`:
//   - `stderr`     — write each event line to stderr (default).
//   - `stdout`     — write each event line to stdout.
//   - `file:///…`  — append to the file at that path.
//   - empty        — defaults to stderr.
//
// The audit handler is intentionally a single subscription per inbound
// event type. Internally it Match()es a set of types and writes via the
// configured sink under a mutex.
type auditConfig struct {
	Sink string `yaml:"sink"`
	// Types overrides the audited event-type set. Default is the
	// control-plane set (auditedTypes); a topology can instead point the
	// same JSONL machinery at any events — e.g. `types: [llm.usage]` for
	// the cost-tracking sink (see pkg/cost and examples/agent.yaml).
	Types []string `yaml:"types" json:"types"`
}

type auditSub struct {
	baseSub

	mu   sync.Mutex
	sink func(line []byte) error

	// matched event types — the control-plane set by default, or the
	// config.types override.
	typeList []string
	types    map[string]bool
}

// auditedTypes returns the set of control-plane event types the audit
// handler reacts to. Exposed for tests. Phase 4c adds the three
// permission event types — grant / revoke / deny — so policy mutations
// land in the same audit stream as topology mutations.
func auditedTypes() []string {
	return []string{
		bus.HandlerRegisteredType,
		bus.SubscribedType,
		bus.UnsubscribedType,
		bus.HandlerDeregisteredType,
		bus.SubscriptionRejectedType,
		bus.PermissionGrantedType,
		bus.PermissionRevokedType,
		bus.PermissionDeniedType,
	}
}

func newAudit(cfg config.HandlerConfig) (bus.Subscriber, error) {
	var ac auditConfig
	if err := decodeConfig(cfg.Config, &ac); err != nil {
		return nil, fmt.Errorf("audit %q: %w", cfg.Name, err)
	}
	sink, err := buildAuditSink(ac.Sink)
	if err != nil {
		return nil, fmt.Errorf("audit %q: %w", cfg.Name, err)
	}
	typeList := ac.Types
	if len(typeList) == 0 {
		typeList = auditedTypes()
	}
	types := map[string]bool{}
	for _, t := range typeList {
		types[t] = true
	}
	return &auditSub{
		baseSub:  baseSub{name: cfg.Name},
		sink:     sink,
		typeList: typeList,
		types:    types,
	}, nil
}

func (a *auditSub) Match(ev event.Event) bool {
	return a.types[ev.Type]
}

// Descriptor exposes the audit handler's full subscription set so the bus
// emits one Subscribed control-plane event per audited type. Bypasses the
// registry's single-Consumes assumption.
func (a *auditSub) Descriptor() bus.HandlerDescriptor {
	d := bus.HandlerDescriptor{
		Name:          a.name,
		MultiConsumes: a.typeList,
		Description:   "audit: log subscribed events to a JSONL sink",
	}
	return d
}

func (a *auditSub) React(_ context.Context, ev event.Event, _ []event.Event) ([]event.Event, error) {
	// Write a single JSONL line capturing type + payload + timestamp. We
	// don't emit any follow-up events — audit is a leaf observer.
	out := map[string]any{
		"type":       ev.Type,
		"id":         ev.ID,
		"ts":         ev.TS,
		"request_id": ev.RequestID,
	}
	var payload any
	if len(ev.Payload) > 0 {
		_ = json.Unmarshal(ev.Payload, &payload)
		out["payload"] = payload
	}
	line, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("audit: marshal: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.sink(line); err != nil {
		return nil, fmt.Errorf("audit: sink: %w", err)
	}
	return nil, nil
}

// buildAuditSink returns a sink function for the configured destination.
func buildAuditSink(sink string) (func([]byte) error, error) {
	switch {
	case sink == "" || sink == "stderr":
		return func(line []byte) error {
			_, err := os.Stderr.Write(append(line, '\n'))
			return err
		}, nil
	case sink == "stdout":
		return func(line []byte) error {
			_, err := os.Stdout.Write(append(line, '\n'))
			return err
		}, nil
	}
	const filePrefix = "file://"
	if len(sink) > len(filePrefix) && sink[:len(filePrefix)] == filePrefix {
		path := sink[len(filePrefix):]
		// Open lazily so the file isn't created until the first write,
		// and append-only so multiple handlers / restarts don't clobber.
		return func(line []byte) error {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = f.Write(append(line, '\n'))
			return err
		}, nil
	}
	return nil, fmt.Errorf("audit: unknown sink %q (use stderr, stdout, or file:///path)", sink)
}

func auditSpec() HandlerSpec {
	return HandlerSpec{
		Type:        "audit",
		Description: "Write control-plane events to a JSONL sink (file/stderr/stdout).",
		Consumes:    "*",
		Emits:       nil,
	}
}
