package handler

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
)

// printerOutput is overridable for tests.
var (
	printerOutputMu sync.Mutex
	printerOutput   io.Writer = os.Stdout
)

// SetPrinterOutput redirects the printer handler's writes. Tests use this to
// capture the assistant message. Returning the previous writer lets a test
// restore the default on cleanup.
func SetPrinterOutput(w io.Writer) io.Writer {
	printerOutputMu.Lock()
	defer printerOutputMu.Unlock()
	prev := printerOutput
	if w == nil {
		w = os.Stdout
	}
	printerOutput = w
	return prev
}

func currentPrinterOutput() io.Writer {
	printerOutputMu.Lock()
	defer printerOutputMu.Unlock()
	return printerOutput
}

type printerConfig struct {
	// Prefix is printed before each emitted line. Defaults to "assistant: ".
	Prefix string `yaml:"prefix"`
	// Field is the payload key to print. Defaults to "text" (the
	// AssistantMessageProposed convention) — set to "result" to print tool
	// outputs instead.
	Field string `yaml:"field"`
}

func newPrinter(cfg config.HandlerConfig) (bus.Subscriber, error) {
	var pc printerConfig
	if err := decodeConfig(cfg.Config, &pc); err != nil {
		return nil, fmt.Errorf("printer %q: %w", cfg.Name, err)
	}
	if pc.Prefix == "" {
		pc.Prefix = "assistant: "
	}
	if pc.Field == "" {
		pc.Field = "text"
	}

	on := cfg.On
	prefix := pc.Prefix
	field := pc.Field
	return &genericSub{
		baseSub: baseSub{name: cfg.Name},
		on:      on,
		run: func(_ context.Context, ev event.Event, _ []event.Event) ([]event.Event, error) {
			// Decode into a permissive map so the printer can read mixed
			// payload shapes (some payloads have ints + strings; the calc
			// AssistantMessageProposed has only strings).
			var p map[string]any
			if err := ev.PayloadAs(&p); err != nil {
				return nil, fmt.Errorf("printer %q: decode payload: %w", cfg.Name, err)
			}
			raw, ok := p[field]
			if !ok {
				return nil, nil
			}
			text := fmt.Sprintf("%v", raw)
			w := currentPrinterOutput()
			if _, err := io.WriteString(w, prefix+text+"\n"); err != nil {
				return nil, err
			}
			return nil, nil
		},
	}, nil
}

// Compile-time interface assertion to catch breakage on refactors.
var _ bus.Subscriber = (*genericSub)(nil)
var _ event.Event
