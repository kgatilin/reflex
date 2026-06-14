package plugin

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// Handler is the plugin author's reaction: given the triggering event, return
// the emits to produce (or an error, which the host turns into a {node}.failed
// fact and keeps draining). It is the whole surface a plugin implements.
type Handler func(ev Event) ([]Emit, error)

// Serve runs the plugin protocol over os.Stdin/os.Stdout until stdin closes (the
// host went away) — the one call a Go plugin's main needs.
//
// IMPORTANT: stdout is the protocol channel. A plugin must write logs/diagnostics
// to os.Stderr ONLY; anything on stdout that is not a Frame corrupts the stream.
func Serve(name string, h Handler) error {
	return serve(name, os.Stdin, os.Stdout, h)
}

// serve is the transport-agnostic loop (tested in-process over pipes).
func serve(name string, r io.Reader, w io.Writer, h Handler) error {
	enc := json.NewEncoder(w)
	dec := json.NewDecoder(bufio.NewReader(r))

	if err := enc.Encode(Frame{Type: TypeHello, Protocol: Protocol, Name: name}); err != nil {
		return fmt.Errorf("plugin %q: sending hello: %w", name, err)
	}
	var welcome Frame
	if err := dec.Decode(&welcome); err != nil {
		return fmt.Errorf("plugin %q: reading welcome: %w", name, err)
	}
	if welcome.Type != TypeWelcome {
		return fmt.Errorf("plugin %q: expected %q, got %q", name, TypeWelcome, welcome.Type)
	}

	for {
		var inv Frame
		if err := dec.Decode(&inv); err != nil {
			if errors.Is(err, io.EOF) {
				return nil // host closed the stream: clean exit
			}
			return fmt.Errorf("plugin %q: reading invoke: %w", name, err)
		}
		if inv.Type != TypeInvoke || inv.Event == nil {
			if err := enc.Encode(Frame{Type: TypeResult, ID: inv.ID, Error: "expected invoke with event"}); err != nil {
				return err
			}
			continue
		}
		res := Frame{Type: TypeResult, ID: inv.ID}
		if emits, err := h(*inv.Event); err != nil {
			res.Error = err.Error()
		} else {
			res.Emits = emits
		}
		if err := enc.Encode(res); err != nil {
			return fmt.Errorf("plugin %q: sending result: %w", name, err)
		}
	}
}
