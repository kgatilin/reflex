package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
)

// Client is the host side of the seam: it drives one plugin over a duplex byte
// stream, one invocation at a time. A mutex serialises the stream because the
// engine may fan a single node across several events in parallel during a drain,
// and those firings share this one pipe; serialising keeps request/result
// correlation trivial (at most one outstanding invoke).
type Client struct {
	enc *json.Encoder
	dec *json.Decoder
	cl  io.Closer // closes the underlying stream / reaps the process; may be nil

	mu   sync.Mutex
	seq  int
	spec Spec
}

// NewClient wraps an already-open duplex stream — r is read from the plugin, w
// is written to it — and completes the hello/welcome handshake. The transport
// (stdio pipes, a socket) is the caller's choice; this is the protocol half.
// closer is invoked by Close (nil for an in-memory transport).
func NewClient(r io.Reader, w io.Writer, closer io.Closer) (*Client, error) {
	c := &Client{
		enc: json.NewEncoder(w),
		dec: json.NewDecoder(bufio.NewReader(r)),
		cl:  closer,
	}
	var hello Frame
	if err := c.dec.Decode(&hello); err != nil {
		return nil, fmt.Errorf("plugin: reading hello: %w", err)
	}
	if hello.Type != TypeHello {
		return nil, fmt.Errorf("plugin: expected %q, got %q", TypeHello, hello.Type)
	}
	if hello.Protocol != Protocol {
		return nil, fmt.Errorf("plugin %q: protocol mismatch (host %d, plugin %d)", hello.Name, Protocol, hello.Protocol)
	}
	c.spec = Spec{Name: hello.Name, Events: hello.Events}
	if err := c.enc.Encode(Frame{Type: TypeWelcome}); err != nil {
		return nil, fmt.Errorf("plugin %q: sending welcome: %w", c.spec.Name, err)
	}
	return c, nil
}

// Spawn starts command[0] (rest as args) and drives it over its stdin/stdout —
// the stdio transport. The child's stderr is inherited so plugin logs surface;
// stdout is the protocol channel and must carry only frames. The process is NOT
// tied to a request context — it lives until Close (or until the daemon exits,
// which closes its stdin and ends Serve's loop).
func Spawn(command ...string) (*Client, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("plugin: empty command")
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("plugin: start %v: %w", command, err)
	}
	c, err := NewClient(stdout, stdin, &procCloser{cmd: cmd, stdin: stdin})
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	return c, nil
}

// Invoke forwards one event to the plugin and returns the emits it produced. A
// plugin-reported error becomes a Go error (the engine turns it into a
// {node}.failed fact and the drain continues). ctx is honoured at the boundary;
// a blocking read mid-call is interrupted only by Close.
func (c *Client) Invoke(ctx context.Context, ev Event) ([]Emit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	id := strconv.Itoa(c.seq)
	if err := c.enc.Encode(Frame{Type: TypeInvoke, ID: id, Event: &ev}); err != nil {
		return nil, fmt.Errorf("plugin %q: sending invoke: %w", c.spec.Name, err)
	}
	var res Frame
	if err := c.dec.Decode(&res); err != nil {
		return nil, fmt.Errorf("plugin %q: reading result: %w", c.spec.Name, err)
	}
	if res.Type != TypeResult {
		return nil, fmt.Errorf("plugin %q: expected %q, got %q", c.spec.Name, TypeResult, res.Type)
	}
	if res.ID != id {
		return nil, fmt.Errorf("plugin %q: result id %q != invoke id %q", c.spec.Name, res.ID, id)
	}
	if res.Error != "" {
		return nil, fmt.Errorf("plugin %q: %s", c.spec.Name, res.Error)
	}
	return res.Emits, nil
}

// Name is the plugin's announced name (from its hello).
func (c *Client) Name() string { return c.spec.Name }

// Spec is the plugin's announced self-description: the kinds it consumes/emits
// and their schemas. The daemon reads it to wire the node and populate the
// catalog dynamically.
func (c *Client) Spec() Spec { return c.spec }

// Close tears the plugin down via the closer supplied at construction.
func (c *Client) Close() error {
	if c.cl == nil {
		return nil
	}
	return c.cl.Close()
}

// procCloser ends a spawned plugin: close its stdin (EOF → Serve returns,
// process exits cleanly), then reap it.
type procCloser struct {
	cmd   *exec.Cmd
	stdin io.Closer
}

func (p *procCloser) Close() error {
	_ = p.stdin.Close()
	return p.cmd.Wait()
}
