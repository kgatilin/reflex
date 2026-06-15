package proxy_test

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kgatilin/reflex/engine"
	"github.com/kgatilin/reflex/nodes/proxy"
)

// TestProxySpawnsAndReacts builds the reflexd multi-call binary and uses the
// "plugin" body kind to spawn `reflexd plugin echo` as a child, then drives one
// React through it — proving the whole seam on the real entrypoint: descriptor
// config -> spawn -> stdio invoke -> emits back as engine.Emit.
func TestProxySpawnsAndReacts(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the reflexd binary; skipped under -short")
	}
	reflexd := buildReflexd(t)
	cfg, err := json.Marshal(proxy.Config{Command: []string{reflexd, "plugin", "echo"}})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	r, err := proxy.Factory("echoer", nil, cfg)
	if err != nil {
		t.Fatalf("Factory: %v", err)
	}

	emits, err := r.React(context.Background(), engine.Event{
		Subject: "app.session.s1.tool.echo.call",
		Payload: json.RawMessage(`{"text":"hi"}`),
	}, nil)
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	if len(emits) != 1 || emits[0].Kind != "echo.reply" {
		t.Fatalf("emits = %+v; want one echo.reply", emits)
	}
	var got struct {
		Subject string `json:"subject"`
	}
	if err := json.Unmarshal(emits[0].Payload, &got); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if got.Subject != "app.session.s1.tool.echo.call" {
		t.Errorf("reply subject = %q; want the triggering subject", got.Subject)
	}
}

// TestFactoryRejectsBadConfig proves the factory fails the changeset on a
// missing command or unparsable config rather than spawning nothing.
func TestFactoryRejectsBadConfig(t *testing.T) {
	cases := map[string]json.RawMessage{
		"empty config":  nil,
		"no command":    json.RawMessage(`{}`),
		"empty command": json.RawMessage(`{"command":[]}`),
		"bad json":      json.RawMessage(`{`),
	}
	for name, cfg := range cases {
		if _, err := proxy.Factory("p", nil, cfg); err == nil {
			t.Errorf("%s: Factory returned nil error", name)
		}
	}
}

func buildReflexd(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "reflexd")
	cmd := exec.Command("go", "build", "-o", out, "github.com/kgatilin/reflex/cmd/reflexd")
	cmd.Env = append(cmd.Environ(), "GOFLAGS=-mod=mod")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build reflexd: %v\n%s", err, b)
	}
	return out
}
