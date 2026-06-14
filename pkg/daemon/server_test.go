package daemon_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/kgatilin/reflex/engine"
	"github.com/kgatilin/reflex/nodes"
	"github.com/kgatilin/reflex/pkg/daemon"
)

// tempSocket returns a unix socket path inside t.TempDir() (auto-cleaned). macOS
// caps sun_path at ~104 bytes, which t.TempDir()'s long path overflows, so we
// chdir into the dir and return a short RELATIVE socket name. The daemon tests
// are sequential (no t.Parallel), so moving and restoring the process cwd is
// safe.
func tempSocket(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	return "d.sock"
}

// TestServerClient_RoundTrip starts a daemon HTTP server on a temp unix socket
// and drives it through the client: apply a declarative document, read the
// topology back, emit one ingress, and confirm the reconciliation reaches the
// terminal answer. This is the daemon's whole transport surface end-to-end.
func TestServerClient_RoundTrip(t *testing.T) {
	nodes.Register("emit", emitFactory)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	socket := tempSocket(t)
	d := daemon.New()
	serveErr := make(chan error, 1)
	go func() { serveErr <- daemon.Serve(ctx, d, socket) }()

	c := daemon.Dial(socket)
	waitReady(t, ctx, c)

	res, err := c.Apply(ctx, declarativeDoc())
	if err != nil {
		t.Fatalf("client Apply: %v", err)
	}
	if !res.Applied {
		t.Fatalf("Apply not applied; report=%+v", res.Report)
	}

	doc, err := c.Topology(ctx)
	if err != nil {
		t.Fatalf("client Topology: %v", err)
	}
	if len(doc.Nodes) != 4 || len(doc.Scopes) != 1 {
		t.Errorf("topology = %d nodes, %d scopes; want 4/1", len(doc.Nodes), len(doc.Scopes))
	}

	events, err := c.Emit(ctx, "app.ingress.cli.task", json.RawMessage(`{}`), true)
	if err != nil {
		t.Fatalf("client Emit: %v", err)
	}
	var answered, closed int
	for _, ev := range events {
		switch engine.KindOf(ev) {
		case "task.answered":
			answered++
		case "scope.request.closed":
			closed++
		}
	}
	if answered != 1 || closed != 1 {
		t.Errorf("reconciliation = %d answered, %d closed; want 1/1", answered, closed)
	}

	cancel()
	if err := <-serveErr; err != nil {
		t.Errorf("Serve returned error: %v", err)
	}
}

// TestServerClient_ApplyRejected proves a disconnected document round-trips as a
// rejection carrying the gap report.
func TestServerClient_ApplyRejected(t *testing.T) {
	nodes.Register("emit", emitFactory)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	socket := tempSocket(t)
	d := daemon.New()
	go func() { _ = daemon.Serve(ctx, d, socket) }()

	c := daemon.Dial(socket)
	waitReady(t, ctx, c)

	bad := declarativeDoc()
	bad.Nodes = bad.Nodes[:1] // drop worker/notify/lifecycle: dead-ends + stalled closure
	res, err := c.Apply(ctx, bad)
	if err == nil {
		t.Fatalf("Apply of a disconnected doc returned nil error")
	}
	if res.Applied {
		t.Errorf("disconnected doc reported Applied=true")
	}
	if res.Report == nil || res.Report.Connected {
		t.Errorf("expected a gap report with Connected=false, got %+v", res.Report)
	}
}

// waitReady polls Topology until the server accepts a connection (the listener
// is up shortly after Serve starts).
func waitReady(t *testing.T, ctx context.Context, c *daemon.Client) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if _, err := c.Topology(ctx); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("daemon did not become ready")
}
