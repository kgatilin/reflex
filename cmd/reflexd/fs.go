// The fs plugin (doc 29 Iteration 3b): rooted file tools —
// tool.fs.{read,edit,write,search} — as a stdio plugin subcommand. The file-op
// logic is ported from the deleted plugins/fs: every path is clamped under the
// root, reads return line-numbered windows, edits are exact unique-old
// replacements guarded by a read-before-edit check.
//
// Stage-0 crutch (carried over): the read-before-edit guard is an in-process
// path → sha map, not the doc-19 fs.seen projection — functionally the same two
// checks (no prior read / stale read).
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	"github.com/kgatilin/reflex/pkg/plugin"
)

const (
	defaultReadLimit = 200  // lines per read window when limit is omitted
	maxReadLimit     = 1000 // hard cap per call — ask again with offset
	defaultSearchMax = 50
	maxSearchMax     = 500
)

// buildFS constructs the fs plugin's spec (its kinds + input schemas, announced
// in hello) and handler, rooted at root (from the node's body_config command).
func buildFS(root string) (plugin.Spec, plugin.Handler, error) {
	if root == "" {
		root = "."
	}
	tools, err := newFSTools(root)
	if err != nil {
		return plugin.Spec{}, nil, err
	}
	return fsSpec, tools.handle, nil
}

// fsSpec is the fs plugin's self-description: four call kinds it consumes (each
// with an LLM-tool-compatible input schema the llm body advertises), and the
// result/failed kinds it emits.
var fsSpec = plugin.Spec{
	Name: "fs",
	Events: []plugin.EventDecl{
		{Kind: "tool.fs.read.call", Role: plugin.RoleIn, Schema: schema(`{
			"type":"object",
			"properties":{
				"path":{"type":"string","description":"path relative to the workspace root"},
				"offset":{"type":"integer","description":"1-based first line (default 1)"},
				"limit":{"type":"integer","description":"max lines to return"}
			},
			"required":["path"]
		}`)},
		{Kind: "tool.fs.edit.call", Role: plugin.RoleIn, Schema: schema(`{
			"type":"object",
			"properties":{
				"path":{"type":"string"},
				"old":{"type":"string","description":"exact text to replace; must be unique unless replace_all"},
				"new":{"type":"string"},
				"replace_all":{"type":"boolean"}
			},
			"required":["path","old","new"]
		}`)},
		{Kind: "tool.fs.write.call", Role: plugin.RoleIn, Schema: schema(`{
			"type":"object",
			"properties":{
				"path":{"type":"string"},
				"content":{"type":"string"}
			},
			"required":["path","content"]
		}`)},
		{Kind: "tool.fs.search.call", Role: plugin.RoleIn, Schema: schema(`{
			"type":"object",
			"properties":{
				"query":{"type":"string"},
				"glob":{"type":"string","description":"optional path/base glob filter"},
				"regex":{"type":"boolean"},
				"max":{"type":"integer"}
			},
			"required":["query"]
		}`)},
		{Kind: "tool.fs.read.result", Role: plugin.RoleOut},
		{Kind: "tool.fs.read.failed", Role: plugin.RoleOut},
		{Kind: "tool.fs.edit.result", Role: plugin.RoleOut},
		{Kind: "tool.fs.edit.failed", Role: plugin.RoleOut},
		{Kind: "tool.fs.write.result", Role: plugin.RoleOut},
		{Kind: "tool.fs.write.failed", Role: plugin.RoleOut},
		{Kind: "tool.fs.search.result", Role: plugin.RoleOut},
		{Kind: "tool.fs.search.failed", Role: plugin.RoleOut},
	},
}

// handle dispatches one call on its kind, runs the op, and emits the matching
// result — or, on an op error (bad path, not found, stale read), the matching
// failed event so the brain can react. A Go error is returned only for a kind
// the node should never have delivered.
func (f *fsTools) handle(ev plugin.Event) ([]plugin.Emit, error) {
	switch ev.Kind {
	case "tool.fs.read.call":
		var p readParams
		return f.run("tool.fs.read", unmarshal(ev.Payload, &p), func() (any, error) { return f.read(p) })
	case "tool.fs.edit.call":
		var p editParams
		return f.run("tool.fs.edit", unmarshal(ev.Payload, &p), func() (any, error) { return f.edit(p) })
	case "tool.fs.write.call":
		var p writeParams
		return f.run("tool.fs.write", unmarshal(ev.Payload, &p), func() (any, error) { return f.write(p) })
	case "tool.fs.search.call":
		var p searchParams
		return f.run("tool.fs.search", unmarshal(ev.Payload, &p), func() (any, error) { return f.search(p) })
	default:
		return nil, fmt.Errorf("fs: unhandled kind %q", ev.Kind)
	}
}

// run turns (decode error | op error | result) into the right emit: <tool>.failed
// on any error, <tool>.result on success.
func (f *fsTools) run(tool string, decodeErr error, op func() (any, error)) ([]plugin.Emit, error) {
	if decodeErr != nil {
		return failed(tool, decodeErr), nil
	}
	res, err := op()
	if err != nil {
		return failed(tool, err), nil
	}
	return []plugin.Emit{{Kind: tool + ".result", Payload: mustJSON(res)}}, nil
}

func failed(tool string, err error) []plugin.Emit {
	return []plugin.Emit{{Kind: tool + ".failed", Payload: mustJSON(map[string]string{"error": err.Error()})}}
}

func unmarshal(b json.RawMessage, v any) error {
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic("fs: marshal: " + err.Error())
	}
	return b
}

func schema(s string) json.RawMessage { return json.RawMessage(s) }

// ---- ported file-op logic (was plugins/fs/tools.go) ----

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

type readParams struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

type readResult struct {
	Path      string    `json:"path"`
	Content   string    `json:"content"`
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
			return nil
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

func globMatch(glob, rel string) bool {
	if strings.Contains(glob, "/") {
		ok, _ := path.Match(glob, rel)
		return ok
	}
	ok, _ := path.Match(glob, path.Base(rel))
	return ok
}

func isBinary(b []byte) bool {
	head := b
	if len(head) > 8192 {
		head = head[:8192]
	}
	return bytes.IndexByte(head, 0) != -1
}
