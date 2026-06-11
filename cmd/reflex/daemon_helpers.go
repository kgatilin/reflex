package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"

	"github.com/google/uuid"

	"github.com/kgatilin/reflex/pkg/event"
	"github.com/kgatilin/reflex/pkg/sdk"
)

// lineEncoder writes one JSON frame per line. Wraps json.Encoder which
// already appends a newline after Encode.
type lineEncoder struct{ enc *json.Encoder }

func newLineEncoder(w io.Writer) *lineEncoder { return &lineEncoder{enc: json.NewEncoder(w)} }
func (e *lineEncoder) encode(f sdk.Frame) error { return e.enc.Encode(f) }

// lineDecoder reads one JSON frame per line.
type lineDecoder struct{ r *bufio.Reader }

func newLineDecoder(r io.Reader) *lineDecoder { return &lineDecoder{r: bufio.NewReaderSize(r, 1<<20)} }
func (d *lineDecoder) decode() (sdk.Frame, error) {
	line, err := d.r.ReadBytes('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && len(line) > 0 {
			return sdk.DecodeFrame(line)
		}
		return sdk.Frame{}, err
	}
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	return sdk.DecodeFrame(line)
}

// buildSeedEvent constructs the partial seed event the daemon's
// EmitAndDrain expects.
func buildSeedEvent(eventType string, payload map[string]any) event.Event {
	raw, _ := json.Marshal(payload)
	return event.Event{
		ID:        uuid.NewString(),
		Type:      eventType,
		RequestID: uuid.NewString(),
		Source:    "cli",
		Payload:   raw,
	}
}
