package analyzer

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Watch implements `analyzer --watch <dir>`: poll a directory for
// .jsonl trace files, recompute the report when a file's mtime moves,
// print the new objective + delta from the previous run for that file.
//
// We poll (1s by default) rather than use fsnotify to keep the dep
// footprint zero — the optimisation loop is the long-term home of any
// reactive design; today this is a watch utility for humans iterating
// on configs.
//
// The "loop" of the optimisation loop is intentionally just an observer
// in Phase 3. Phase 6 will replace the body of OnChange with a config
// rewriter that emits new YAML and re-runs reflex.
type Watcher struct {
	Dir      string
	Interval time.Duration
	Out      io.Writer
	JSON     bool
	// prev tracks last-seen mtime + objective per file so we can compute
	// a delta on the next change.
	prev map[string]watchState
}

type watchState struct {
	mtime     time.Time
	objective float64
}

// NewWatcher constructs a Watcher with sane defaults. The CLI fills in
// Dir + JSON; Out defaults to os.Stdout.
func NewWatcher(dir string, jsonOut bool, out io.Writer) *Watcher {
	if out == nil {
		out = os.Stdout
	}
	return &Watcher{
		Dir:      dir,
		Interval: time.Second,
		Out:      out,
		JSON:     jsonOut,
		prev:     map[string]watchState{},
	}
}

// Run polls until ctx is cancelled. The first pass establishes baselines
// (silent for unchanged files); subsequent passes print one line per
// file whose mtime moved.
func (w *Watcher) Run(ctx context.Context) error {
	if w.Interval <= 0 {
		w.Interval = time.Second
	}
	// Initial pass: establish baselines and print them.
	if err := w.sweep(true); err != nil {
		return err
	}
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := w.sweep(false); err != nil {
				// Print the error but keep watching — a single read
				// failure (truncated file mid-write) shouldn't crash
				// the loop.
				fmt.Fprintln(w.Out, "watch error:", err)
			}
		}
	}
}

// sweep lists *.jsonl in Dir, analyses the ones whose mtime changed
// since last sweep, and prints the result. initial=true prints baseline
// for every file (so the first pass is visible).
func (w *Watcher) sweep(initial bool) error {
	entries, err := os.ReadDir(w.Dir)
	if err != nil {
		return fmt.Errorf("read dir %s: %w", w.Dir, err)
	}
	paths := []string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(e.Name()), ".jsonl") {
			continue
		}
		paths = append(paths, filepath.Join(w.Dir, e.Name()))
	}
	sort.Strings(paths)
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		prev, hadPrev := w.prev[p]
		if !initial && hadPrev && !fi.ModTime().After(prev.mtime) {
			continue
		}
		tr, err := ReadTraceFile(p)
		if err != nil {
			fmt.Fprintln(w.Out, "watch: read", p, "->", err)
			continue
		}
		rep, err := Analyze(tr)
		if err != nil {
			fmt.Fprintln(w.Out, "watch: analyze", p, "->", err)
			continue
		}
		delta := ""
		if hadPrev {
			d := rep.Objective - prev.objective
			delta = fmt.Sprintf(" (Δ=%+g)", d)
		} else {
			delta = " (baseline)"
		}
		if w.JSON {
			// JSON output includes the full report — a tail-able stream
			// of structured updates.
			if err := rep.PrintJSON(w.Out); err != nil {
				return err
			}
		} else {
			fmt.Fprintf(w.Out,
				"%s  %s  objective=%g%s  depth=%d  orphans=%d  cycles=%d\n",
				time.Now().Format(time.RFC3339),
				p, rep.Objective, delta,
				maxDepth(rep.Metrics), len(rep.Metrics.Orphans),
				rep.Cycles.CyclingNodes)
		}
		w.prev[p] = watchState{mtime: fi.ModTime(), objective: rep.Objective}
	}
	return nil
}
