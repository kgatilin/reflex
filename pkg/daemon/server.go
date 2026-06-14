package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"

	"github.com/kgatilin/reflex/engine"
	"github.com/kgatilin/reflex/pkg/topology"
)

// The daemon's API is HTTP/JSON over a unix socket (a local, port-less
// transport; the out-of-process PLUGIN socket of doc 20 is a later concern).
// Every route is one of two verbs (doc 20 "the daemon surface"): emit a
// changeset / event, or read a view. The handler is a thin shell over the
// Daemon's in-process methods — the engine's single-writer discipline is held by
// the Daemon mutex, so concurrent HTTP requests serialize correctly.

// applyResponse / emitResponse / validateResponse are the JSON envelopes.
type applyResponse struct {
	Applied bool           `json:"applied"`
	Report  *engine.Report `json:"report,omitempty"`
	Error   string         `json:"error,omitempty"`
}

type emitResponse struct {
	Events []engine.Event `json:"events"`
}

type validateResponse struct {
	Report engine.Report `json:"report"`
}

type emitRequest struct {
	Subject string          `json:"subject"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Drain   bool            `json:"drain"`
}

// Handler returns the daemon's HTTP routes.
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /apply", d.handleApply)
	mux.HandleFunc("POST /validate", d.handleValidate)
	mux.HandleFunc("POST /emit", d.handleEmit)
	mux.HandleFunc("GET /topology", d.handleTopology)
	mux.HandleFunc("GET /events", d.handleEvents)
	return mux
}

// handleApply parses a topology document (YAML or JSON body) and applies it. A
// connectivity rejection returns 422 with the gap Report; a body/parse error
// returns 400; success returns 200.
func (d *Daemon) handleApply(w http.ResponseWriter, r *http.Request) {
	doc, err := readDoc(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, applyResponse{Error: err.Error()})
		return
	}
	err = d.Apply(r.Context(), doc)
	if err == nil {
		writeJSON(w, http.StatusOK, applyResponse{Applied: true})
		return
	}
	var ve *engine.ValidationError
	if errors.As(err, &ve) {
		writeJSON(w, http.StatusUnprocessableEntity, applyResponse{Applied: false, Report: &ve.Report, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusUnprocessableEntity, applyResponse{Applied: false, Error: err.Error()})
}

func (d *Daemon) handleValidate(w http.ResponseWriter, r *http.Request) {
	doc, err := readDoc(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, applyResponse{Error: err.Error()})
		return
	}
	rep, err := d.Validate(doc)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, applyResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, validateResponse{Report: rep})
}

func (d *Daemon) handleEmit(w http.ResponseWriter, r *http.Request) {
	var req emitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, applyResponse{Error: err.Error()})
		return
	}
	if req.Subject == "" {
		writeJSON(w, http.StatusBadRequest, applyResponse{Error: "emit: subject is required"})
		return
	}
	events, err := d.Emit(r.Context(), req.Subject, req.Payload, req.Drain)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, applyResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, emitResponse{Events: events})
}

func (d *Daemon) handleTopology(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, topology.FromDecls(d.Topology()))
}

func (d *Daemon) handleEvents(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, d.Events())
}

// readDoc parses the request body as a topology document (YAML or JSON).
func readDoc(r *http.Request) (topology.Document, error) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return topology.Document{}, err
	}
	return topology.Parse(data)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Serve hosts the daemon's HTTP API on a unix socket at path until the context
// is cancelled. A stale socket file at path is removed first (a previous run's
// leftover); the socket is removed on shutdown.
func Serve(ctx context.Context, d *Daemon, socketPath string) error {
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("daemon: clear stale socket %s: %w", socketPath, err)
	}
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("daemon: listen %s: %w", socketPath, err)
	}
	srv := &http.Server{Handler: d.Handler()}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	err = srv.Serve(l)
	_ = os.Remove(socketPath)
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}
