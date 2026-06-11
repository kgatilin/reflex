// The fs plugin: doc 14's file tools as an out-of-process SDK handler —
// tool.fs.read / tool.fs.edit / tool.fs.write / tool.fs.search. Every path is
// clamped under --root; reads return line-numbered windows; edits are exact
// unique-old replacements guarded by the read-before-edit check.
//
// Stage-0 crutch (doc 22): the read-before-edit guard is an in-process
// path → sha map instead of the doc-19 `fs.seen` projection — functionally
// the same two checks (no prior read / stale read). The honest projection
// replaces this once reflex has built doc 19 itself.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

const (
	defaultReadLimit = 200  // lines per read window when limit is omitted
	maxReadLimit     = 1000 // hard cap per call — ask again with offset
	defaultSearchMax = 50
	maxSearchMax     = 500
)

type fsTools struct {
	root string

	mu   sync.Mutex
	seen map[string]string // rel path → sha at last read/edit/write
}

func newFSTools(root string) (*fsTools, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("fs: root %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("fs: root %q is not a directory", root)
	}
	return &fsTools{root: abs, seen: map[string]string{}}, nil
}

// resolve clamps a model-supplied path under root. Absolute paths and any
// `..` escape are rejected — out-of-scope becomes a failed event upstream,
// never an access.
func (f *fsTools) resolve(p string) (abs, rel string, err error) {
	if p == "" {
		return "", "", errors.New("path is required")
	}
	if filepath.IsAbs(p) {
		return "", "", fmt.Errorf("path %q must be relative to the workspace root", p)
	}
	rel = filepath.ToSlash(filepath.Clean("/" + p))[1:] // clamp: no ..-escape survives
	abs = filepath.Join(f.root, filepath.FromSlash(rel))
	if abs != f.root && !strings.HasPrefix(abs, f.root+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path %q escapes the workspace root", p)
	}
	return abs, rel, nil
}

func contentSHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// --- read ---

type readParams struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"` // 1-based first line, default 1
	Limit  int    `json:"limit"`  // lines, default defaultReadLimit
}

type readResult struct {
	Path      string    `json:"path"`
	Content   string    `json:"content"` // line-numbered window
	SHA       string    `json:"sha"`
	Lines     lineRange `json:"lines"`
	Truncated bool      `json:"truncated"`
}

type lineRange struct {
	From  int `json:"from"`
	To    int `json:"to"`
	Total int `json:"total"`
}

func (f *fsTools) read(p readParams) (readResult, error) {
	abs, rel, err := f.resolve(p.Path)
	if err != nil {
		return readResult{}, err
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return readResult{}, fmt.Errorf("read %q: %w", rel, err)
	}
	sha := contentSHA(raw)

	lines := strings.Split(string(raw), "\n")
	// A trailing newline yields one empty trailing element; don't count it
	// as a line of content.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	total := len(lines)

	from := p.Offset
	if from < 1 {
		from = 1
	}
	limit := p.Limit
	if limit <= 0 {
		limit = defaultReadLimit
	}
	if limit > maxReadLimit {
		limit = maxReadLimit
	}
	if from > total {
		return readResult{}, fmt.Errorf("read %q: offset %d beyond end of file (%d lines)", rel, from, total)
	}
	to := from + limit - 1
	if to > total {
		to = total
	}

	var b strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "%6d\t%s\n", i, lines[i-1])
	}

	f.mu.Lock()
	f.seen[rel] = sha
	f.mu.Unlock()

	return readResult{
		Path:      rel,
		Content:   b.String(),
		SHA:       sha,
		Lines:     lineRange{From: from, To: to, Total: total},
		Truncated: to < total,
	}, nil
}

// --- edit ---

type editParams struct {
	Path       string `json:"path"`
	Old        string `json:"old"`
	New        string `json:"new"`
	ReplaceAll bool   `json:"replace_all"`
}

type editResult struct {
	Path     string `json:"path"`
	SHA      string `json:"sha"`
	Replaced int    `json:"replaced"`
}

func (f *fsTools) edit(p editParams) (editResult, error) {
	abs, rel, err := f.resolve(p.Path)
	if err != nil {
		return editResult{}, err
	}
	if p.Old == "" {
		return editResult{}, errors.New("old is required and must be non-empty")
	}
	if p.Old == p.New {
		return editResult{}, errors.New("old and new are identical")
	}

	raw, err := os.ReadFile(abs)
	if err != nil {
		return editResult{}, fmt.Errorf("edit %q: %w", rel, err)
	}

	// Read-before-edit guard, both halves (doc 14).
	f.mu.Lock()
	seen, ok := f.seen[rel]
	f.mu.Unlock()
	if !ok {
		return editResult{}, fmt.Errorf("edit %q: no prior read — read the file first", rel)
	}
	if seen != contentSHA(raw) {
		return editResult{}, fmt.Errorf("edit %q: stale read — the file changed on disk, re-read it", rel)
	}

	content := string(raw)
	count := strings.Count(content, p.Old)
	switch {
	case count == 0:
		return editResult{}, fmt.Errorf("edit %q: old string not found", rel)
	case count > 1 && !p.ReplaceAll:
		return editResult{}, fmt.Errorf("edit %q: old string matches %d times — extend it to be unique or set replace_all", rel, count)
	}

	replaced := 1
	if p.ReplaceAll {
		replaced = count
		content = strings.ReplaceAll(content, p.Old, p.New)
	} else {
		content = strings.Replace(content, p.Old, p.New, 1)
	}

	mode := fs.FileMode(0o644)
	if info, err := os.Stat(abs); err == nil {
		mode = info.Mode()
	}
	if err := os.WriteFile(abs, []byte(content), mode); err != nil {
		return editResult{}, fmt.Errorf("edit %q: %w", rel, err)
	}
	sha := contentSHA([]byte(content))
	f.mu.Lock()
	f.seen[rel] = sha
	f.mu.Unlock()
	return editResult{Path: rel, SHA: sha, Replaced: replaced}, nil
}

// --- write ---

type writeParams struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type writeResult struct {
	Path string `json:"path"`
	SHA  string `json:"sha"`
}

func (f *fsTools) write(p writeParams) (writeResult, error) {
	abs, rel, err := f.resolve(p.Path)
	if err != nil {
		return writeResult{}, err
	}
	// write is create / wholesale overwrite. Overwriting an existing file
	// the context has never seen is the same hazard edit guards against,
	// so the stale-read half applies once the file exists.
	if raw, err := os.ReadFile(abs); err == nil {
		f.mu.Lock()
		seen, ok := f.seen[rel]
		f.mu.Unlock()
		if !ok {
			return writeResult{}, fmt.Errorf("write %q: file exists and was never read — read it first or pick a new path", rel)
		}
		if seen != contentSHA(raw) {
			return writeResult{}, fmt.Errorf("write %q: stale read — the file changed on disk, re-read it", rel)
		}
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return writeResult{}, fmt.Errorf("write %q: %w", rel, err)
	}
	if err := os.WriteFile(abs, []byte(p.Content), 0o644); err != nil {
		return writeResult{}, fmt.Errorf("write %q: %w", rel, err)
	}
	sha := contentSHA([]byte(p.Content))
	f.mu.Lock()
	f.seen[rel] = sha
	f.mu.Unlock()
	return writeResult{Path: rel, SHA: sha}, nil
}

// --- search ---

type searchParams struct {
	Query string `json:"query"`
	Glob  string `json:"glob"`
	Regex bool   `json:"regex"`
	Max   int    `json:"max"`
}

type searchResult struct {
	Matches   []searchMatch `json:"matches"`
	Truncated bool          `json:"truncated"`
}

type searchMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// skippedDirs are never descended into: VCS internals and dependency caches
// are noise the model should reach via go tools, not file search.
var skippedDirs = map[string]bool{".git": true, "node_modules": true, ".idea": true, ".vscode": true}

func (f *fsTools) search(p searchParams) (searchResult, error) {
	if p.Query == "" {
		return searchResult{}, errors.New("query is required")
	}
	max := p.Max
	if max <= 0 {
		max = defaultSearchMax
	}
	if max > maxSearchMax {
		max = maxSearchMax
	}

	match := func(line string) bool { return strings.Contains(line, p.Query) }
	if p.Regex {
		re, err := regexp.Compile(p.Query)
		if err != nil {
			return searchResult{}, fmt.Errorf("bad regex: %w", err)
		}
		match = re.MatchString
	}

	var (
		out       searchResult
		truncated bool
	)
	err := filepath.WalkDir(f.root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped, not fatal
		}
		if d.IsDir() {
			if skippedDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(f.root, abs)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if p.Glob != "" && !globMatch(p.Glob, rel) {
			return nil
		}
		raw, err := os.ReadFile(abs)
		if err != nil || isBinary(raw) {
			return nil
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if !match(line) {
				continue
			}
			if len(out.Matches) >= max {
				truncated = true
				return filepath.SkipAll
			}
			out.Matches = append(out.Matches, searchMatch{Path: rel, Line: i + 1, Text: strings.TrimSpace(line)})
		}
		return nil
	})
	if err != nil {
		return searchResult{}, err
	}
	sort.SliceStable(out.Matches, func(i, j int) bool {
		if out.Matches[i].Path != out.Matches[j].Path {
			return out.Matches[i].Path < out.Matches[j].Path
		}
		return out.Matches[i].Line < out.Matches[j].Line
	})
	out.Truncated = truncated
	return out, nil
}

// globMatch matches rel (slash-separated) against the glob: a pattern with a
// slash matches the whole relative path, otherwise just the base name.
func globMatch(glob, rel string) bool {
	if strings.Contains(glob, "/") {
		ok, _ := path.Match(glob, rel)
		return ok
	}
	ok, _ := path.Match(glob, path.Base(rel))
	return ok
}

// isBinary applies the classic NUL-byte sniff to the head of the file.
func isBinary(b []byte) bool {
	head := b
	if len(head) > 8192 {
		head = head[:8192]
	}
	return bytes.IndexByte(head, 0) != -1
}
