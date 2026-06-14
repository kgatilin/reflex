package plugin

import (
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestServeClientRoundTrip wires a Serve loop to a Client over two in-memory
// pipes (no process) and drives two invocations through the full handshake +
// invoke/result protocol — the seam's behaviour with the transport factored out.
func TestServeClientRoundTrip(t *testing.T) {
	h2pR, h2pW := io.Pipe() // host -> plugin
	p2hR, p2hW := io.Pipe() // plugin -> host

	served := make(chan error, 1)
	go func() {
		served <- serve("echo", h2pR, p2hW, func(ev Event) ([]Emit, error) {
			return []Emit{{Kind: "echo.reply", Payload: ev.Payload}}, nil
		})
	}()

	c, err := NewClient(p2hR, h2pW, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.Name() != "echo" {
		t.Errorf("Name = %q, want echo", c.Name())
	}

	for i, in := range []string{`{"n":1}`, `{"n":2}`} {
		emits, err := c.Invoke(context.Background(), Event{Subject: "app.test", Payload: json.RawMessage(in)})
		if err != nil {
			t.Fatalf("Invoke %d: %v", i, err)
		}
		if len(emits) != 1 || emits[0].Kind != "echo.reply" || string(emits[0].Payload) != in {
			t.Fatalf("Invoke %d emits = %+v; want one echo.reply with %s", i, emits, in)
		}
	}

	h2pW.Close() // EOF to the plugin: Serve returns cleanly
	if err := <-served; err != nil {
		t.Errorf("serve returned %v; want nil on EOF", err)
	}
}

// TestInvokeErrorPropagates proves a handler error crosses the wire as a Go
// error (the engine turns it into a {node}.failed fact).
func TestInvokeErrorPropagates(t *testing.T) {
	h2pR, h2pW := io.Pipe()
	p2hR, p2hW := io.Pipe()
	go func() {
		_ = serve("boom", h2pR, p2hW, func(Event) ([]Emit, error) {
			return nil, io.ErrUnexpectedEOF // any error
		})
	}()
	c, err := NewClient(p2hR, h2pW, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = c.Invoke(context.Background(), Event{Subject: "x"})
	if err == nil {
		t.Fatal("Invoke returned nil error for a failing handler")
	}
	h2pW.Close()
}

// TestNewClientRejectsNonHello proves the handshake fails fast when the first
// frame is not a hello.
func TestNewClientRejectsNonHello(t *testing.T) {
	p2hR, p2hW := io.Pipe()
	_, hostW := io.Pipe()
	go func() {
		enc := json.NewEncoder(p2hW)
		_ = enc.Encode(Frame{Type: TypeWelcome}) // wrong: should be hello
		p2hW.Close()
	}()
	if _, err := NewClient(p2hR, hostW, nil); err == nil {
		t.Fatal("NewClient accepted a non-hello first frame")
	}
}

// TestSpawnEchoPlugin builds the testdata echo plugin and drives it as a real
// child process over stdio — proving the Spawn transport, not just the protocol.
func TestSpawnEchoPlugin(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a child binary; skipped under -short")
	}
	bin := buildBin(t, "github.com/kgatilin/reflex/pkg/plugin/testdata/echoplugin")
	c, err := Spawn(bin)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer c.Close()

	emits, err := c.Invoke(context.Background(), Event{Subject: "app.s.x.thing", Payload: json.RawMessage(`{"k":"v"}`)})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(emits) != 1 || emits[0].Kind != "echo.reply" || string(emits[0].Payload) != `{"k":"v"}` {
		t.Fatalf("emits = %+v; want one echo.reply echoing the payload", emits)
	}
}

// buildBin compiles a module package to a temp binary and returns its path.
func buildBin(t *testing.T, importPath string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "plugin.bin")
	cmd := exec.Command("go", "build", "-o", out, importPath)
	cmd.Env = append(cmd.Environ(), "GOFLAGS=-mod=mod")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", importPath, err, b)
	}
	return out
}
