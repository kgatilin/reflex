package handler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kgatilin/reflex/pkg/bus"
	"github.com/kgatilin/reflex/pkg/config"
	"github.com/kgatilin/reflex/pkg/event"
)

// echo is a minimal handler that re-emits the payload of its trigger event
// under a new event type. Useful for stall scenarios and as the simplest
// possible subscriber example.
type echoConfig struct {
	Emit string `yaml:"emit"`
}

func newEcho(cfg config.HandlerConfig) (bus.Subscriber, error) {
	var ec echoConfig
	if err := decodeConfig(cfg.Config, &ec); err != nil {
		return nil, fmt.Errorf("echo %q: %w", cfg.Name, err)
	}
	if ec.Emit == "" {
		return nil, fmt.Errorf("echo %q: emit is required", cfg.Name)
	}

	on := cfg.On
	emit := ec.Emit
	return &genericSub{
		baseSub: baseSub{name: cfg.Name},
		on:      on,
		run: func(_ context.Context, ev event.Event, _ []event.Event) ([]event.Event, error) {
			// Re-wrap whatever the source payload was.
			var raw map[string]any
			_ = ev.PayloadAs(&raw)
			payload, err := json.Marshal(raw)
			if err != nil {
				return nil, err
			}
			return []event.Event{{
				Type:    emit,
				Payload: payload,
			}}, nil
		},
	}, nil
}
