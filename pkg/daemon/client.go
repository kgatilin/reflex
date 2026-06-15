package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/kgatilin/reflex/engine"
	"github.com/kgatilin/reflex/pkg/topology"
)

// Client talks to a daemon over its unix socket — the transport behind the CLI
// and the external harness. The HTTP host is a placeholder; the dialer ignores
// it and connects to the socket path.
type Client struct {
	http *http.Client
	base string
}

// Dial returns a client bound to the daemon's unix socket at path.
func Dial(socketPath string) *Client {
	return &Client{
		base: "http://unix",
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// Apply sends a topology document to the daemon. A rejection returns an error
// carrying the daemon's reason (and the gap Report is in the response body).
func (c *Client) Apply(ctx context.Context, doc topology.Document) (applyResult, error) {
	body, err := json.Marshal(doc)
	if err != nil {
		return applyResult{}, err
	}
	var resp applyResponse
	status, err := c.do(ctx, http.MethodPost, "/apply", body, &resp)
	if err != nil {
		return applyResult{}, err
	}
	res := applyResult{Applied: resp.Applied, Report: resp.Report}
	if status != http.StatusOK {
		if resp.Error != "" {
			return res, fmt.Errorf("apply rejected: %s", resp.Error)
		}
		return res, fmt.Errorf("apply rejected (status %d)", status)
	}
	return res, nil
}

// applyResult is the client-side outcome of Apply: accepted or rejected, with
// the gap report when rejected.
type applyResult struct {
	Applied bool
	Report  *engine.Report
}

// Validate dry-runs a document against the daemon's live table.
func (c *Client) Validate(ctx context.Context, doc topology.Document) (engine.Report, error) {
	body, err := json.Marshal(doc)
	if err != nil {
		return engine.Report{}, err
	}
	var resp validateResponse
	if _, err := c.do(ctx, http.MethodPost, "/validate", body, &resp); err != nil {
		return engine.Report{}, err
	}
	return resp.Report, nil
}

// Emit sends one external event and returns the events produced (drain controls
// whether the daemon drives to quiescence before replying).
func (c *Client) Emit(ctx context.Context, subject string, payload json.RawMessage, drain bool) ([]engine.Event, error) {
	body, err := json.Marshal(emitRequest{Subject: subject, Payload: payload, Drain: drain})
	if err != nil {
		return nil, err
	}
	var resp emitResponse
	if _, err := c.do(ctx, http.MethodPost, "/emit", body, &resp); err != nil {
		return nil, err
	}
	return resp.Events, nil
}

// Topology reads the daemon's live table as a document.
func (c *Client) Topology(ctx context.Context) (topology.Document, error) {
	var doc topology.Document
	if _, err := c.do(ctx, http.MethodGet, "/topology", nil, &doc); err != nil {
		return topology.Document{}, err
	}
	return doc, nil
}

// Events reads the daemon's whole log.
func (c *Client) Events(ctx context.Context) ([]engine.Event, error) {
	var events []engine.Event
	if _, err := c.do(ctx, http.MethodGet, "/events", nil, &events); err != nil {
		return nil, err
	}
	return events, nil
}

// do performs one request, decoding the JSON response into out (when non-nil).
// It returns the HTTP status so callers can distinguish accepted from rejected.
func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("daemon: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
			return resp.StatusCode, fmt.Errorf("daemon: decode %s response: %w", path, err)
		}
	}
	return resp.StatusCode, nil
}
