package main

import (
	"context"
	"strings"
	"testing"

	"github.com/kgatilin/reflex/pkg/handler"
)

// TestEmitAggregateExample drives the aggregate.yaml example via the
// executeRun pipeline (the same path `reflex emit` takes) and asserts the
// aggregator fires once. End-to-end coverage of the new emit/wait surface.
func TestEmitAggregateExample(t *testing.T) {
	var sink strings.Builder
	prev := handler.SetPrinterOutput(&sink)
	t.Cleanup(func() { handler.SetPrinterOutput(prev) })

	res, err := executeRun(context.Background(),
		examplePath(t, "aggregate.yaml"),
		"ClassifyRequested",
		map[string]any{"item": "foo"})
	if err != nil {
		t.Fatalf("executeRun: %v", err)
	}
	count := 0
	for _, e := range res.Events {
		if e.Type == "ClassificationsAggregated" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("ClassificationsAggregated count = %d, want 1", count)
	}
	if !strings.Contains(sink.String(), "aggregated count: 3") {
		t.Fatalf("printer output = %q", sink.String())
	}
}

// TestEmitDrainWaitPredicate confirms the drain wait-predicate resolves
// after executeRun returns (the bus drained, DrainQuiesced fired).
func TestEmitDrainWaitPredicate(t *testing.T) {
	res, err := executeRun(context.Background(),
		examplePath(t, "aggregate.yaml"),
		"ClassifyRequested",
		map[string]any{"item": "foo"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, why := checkWaitPredicate("drain", res); !ok {
		t.Fatalf("drain predicate failed: %s", why)
	}
}

// TestFindEventByCommand verifies the YAML cli.command binding lookup —
// the engine that lets `reflex invoke classify` map to ClassifyRequested.
func TestFindEventByCommand(t *testing.T) {
	cfg, err := loadConfig(examplePath(t, "aggregate.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	ec, name := findEventByCommand(cfg, "invoke classify")
	if ec == nil {
		t.Fatal("expected to find event bound to `invoke classify`")
	}
	if name != "ClassifyRequested" {
		t.Fatalf("name = %q, want ClassifyRequested", name)
	}
	if ec.CLI == nil || ec.CLI.Wait != "drain" {
		t.Fatalf("CLI.Wait = %+v", ec.CLI)
	}
}
