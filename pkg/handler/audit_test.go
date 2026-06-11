package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
)

func TestAuditWritesEachControlPlaneEvent(t *testing.T) {
	var buf bytes.Buffer
	sub, err := newAudit(config.HandlerConfig{
		Name:   "audit",
		Type:   "audit",
		On:     bus.HandlerRegisteredType,
		Config: map[string]any{"sink": "stderr"},
	})
	if err != nil {
		t.Fatalf("newAudit: %v", err)
	}
	// Swap the sink to capture into buf.
	a := sub.(*auditSub)
	a.sink = func(line []byte) error {
		buf.Write(append(line, '\n'))
		return nil
	}

	for _, typ := range auditedTypes() {
		_, err := a.React(context.Background(), event.Event{
			ID:   "id-" + typ,
			Type: typ,
		}, nil)
		if err != nil {
			t.Fatalf("React %s: %v", typ, err)
		}
	}
	got := buf.String()
	for _, typ := range auditedTypes() {
		if !strings.Contains(got, typ) {
			t.Fatalf("audit log missing %s; got: %s", typ, got)
		}
	}
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != len(auditedTypes()) {
		t.Fatalf("audit lines = %d, want %d", len(lines), len(auditedTypes()))
	}
}

func TestAuditFileSink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	sub, err := newAudit(config.HandlerConfig{
		Name:   "audit",
		Type:   "audit",
		On:     bus.HandlerRegisteredType,
		Config: map[string]any{"sink": "file://" + path},
	})
	if err != nil {
		t.Fatalf("newAudit: %v", err)
	}
	_, err = sub.React(context.Background(), event.Event{
		ID:      "abc",
		Type:    bus.HandlerRegisteredType,
		Payload: []byte(`{"name":"h1"}`),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), bus.HandlerRegisteredType) {
		t.Fatalf("file contents = %s", data)
	}
	var parsed map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &parsed); err != nil {
		t.Fatalf("audit line is not valid JSON: %v", err)
	}
}

func TestAuditUnknownSinkErrors(t *testing.T) {
	_, err := newAudit(config.HandlerConfig{
		Name:   "audit",
		Type:   "audit",
		On:     bus.HandlerRegisteredType,
		Config: map[string]any{"sink": "kafka://broker"},
	})
	if err == nil {
		t.Fatal("expected unknown-sink error")
	}
}

func TestAuditDescriptorReportsMultiConsumes(t *testing.T) {
	sub, err := newAudit(config.HandlerConfig{
		Name: "audit", Type: "audit", On: bus.HandlerRegisteredType,
	})
	if err != nil {
		t.Fatal(err)
	}
	d, ok := sub.(interface {
		Descriptor() bus.HandlerDescriptor
	})
	if !ok {
		t.Fatal("audit must expose Descriptor")
	}
	desc := d.Descriptor()
	if len(desc.MultiConsumes) != len(auditedTypes()) {
		t.Fatalf("MultiConsumes = %v, want %v", desc.MultiConsumes, auditedTypes())
	}
}
