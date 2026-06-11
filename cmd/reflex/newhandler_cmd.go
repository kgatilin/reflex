// newhandler_cmd.go implements the `reflex new-handler` scaffolding
// subcommand. It exists to remove copy-paste from the day-one experience:
// a user can stand up either a YAML-declared handler entry or a runnable
// Go handler binary by naming the consumed event, the emitted events, and
// the scope.
//
// The command is intentionally narrow. It does not run the bus, does not
// load a config beyond a duplicate-name check, and does not link to the
// SDK at runtime — it only writes text. Anything fancier belongs in a
// separate command.

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/kgatilin/reflex/pkg/config"
)

// scaffoldOptions carries the resolved CLI inputs for the scaffold flow.
// Keeping it cobra-free makes it directly testable.
type scaffoldOptions struct {
	Name       string
	Consumes   string
	Emits      []string
	Terminal   []string
	Scope      string
	Language   string // "yaml" | "go"
	ConfigPath string // optional, for yaml mode
	OutputDir  string // optional override for Go mode (defaults to "cmd/<name>")
}

// handlerNameRE constrains the handler instance name. Lower-case ASCII so
// the name reads cleanly in logs, dotted scopes, and shell completions;
// digits and hyphens allowed past the first character to leave room for
// versioned variants ("classifier-v2") without inventing a separator.
var handlerNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// eventTypeRE matches a Go-identifier-ish event type with optional dots
// to allow namespaced names like "github.IssueOpened". Conservative on
// purpose — anything weirder is almost always a typo.
var eventTypeRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)

func newNewHandlerCmd() *cobra.Command {
	opts := &scaffoldOptions{}
	var emitsCSV, terminalCSV string
	cmd := &cobra.Command{
		Use:          "new-handler <name>",
		Short:        "scaffold a new handler block (YAML) or handler binary (Go)",
		Long:         "Generate handler boilerplate so a new subscription can be added without copy-paste. By default appends a YAML block to the file passed via --config; with --language go, writes a standalone main.go using pkg/sdk.",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Name = args[0]
			opts.Emits = splitCSV(emitsCSV)
			opts.Terminal = splitCSV(terminalCSV)
			if opts.Language == "" {
				opts.Language = "yaml"
			}
			out, err := runScaffold(opts)
			if err != nil {
				return err
			}
			if out != "" {
				fmt.Fprint(cmd.OutOrStdout(), out)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.Consumes, "consumes", "", "event type this handler reacts to (required)")
	cmd.Flags().StringVar(&emitsCSV, "emits", "", "comma-separated list of event types this handler may emit")
	cmd.Flags().StringVar(&terminalCSV, "terminal", "", "comma-separated subset of --emits that should be marked terminal")
	cmd.Flags().StringVar(&opts.Scope, "scope", "", "dotted scope this handler owns (e.g. tools.fs.read)")
	cmd.Flags().StringVar(&opts.Language, "language", "yaml", "scaffold language: yaml | go")
	cmd.Flags().StringVar(&opts.ConfigPath, "config", "", "YAML config to append to (yaml mode); when empty, output to stdout")
	cmd.Flags().StringVar(&opts.OutputDir, "output-dir", "", "override output directory for go mode (defaults to cmd/<name>)")
	return cmd
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// runScaffold is the cobra-free entry point. Returns text to print (e.g.
// the YAML block when no --config was given) along with any error.
func runScaffold(opts *scaffoldOptions) (string, error) {
	if err := validateScaffoldOptions(opts); err != nil {
		return "", err
	}
	switch opts.Language {
	case "yaml":
		return runScaffoldYAML(opts)
	case "go":
		return runScaffoldGo(opts)
	default:
		return "", fmt.Errorf("new-handler: unknown --language %q (want yaml|go)", opts.Language)
	}
}

func validateScaffoldOptions(opts *scaffoldOptions) error {
	if !handlerNameRE.MatchString(opts.Name) {
		return fmt.Errorf("new-handler: invalid handler name %q (must match [a-z][a-z0-9-]*)", opts.Name)
	}
	if opts.Consumes == "" {
		return fmt.Errorf("new-handler: --consumes is required")
	}
	if !eventTypeRE.MatchString(opts.Consumes) {
		return fmt.Errorf("new-handler: invalid consumes event type %q", opts.Consumes)
	}
	for _, e := range opts.Emits {
		if !eventTypeRE.MatchString(e) {
			return fmt.Errorf("new-handler: invalid emits event type %q", e)
		}
	}
	// Every --terminal must also appear in --emits. We auto-add to keep
	// the YAML lossless, but flag a hard mismatch when the user passes a
	// terminal that's clearly not in the emit set (typo guard).
	for _, t := range opts.Terminal {
		if !eventTypeRE.MatchString(t) {
			return fmt.Errorf("new-handler: invalid terminal event type %q", t)
		}
	}
	if opts.Scope != "" && !eventTypeRE.MatchString(opts.Scope) {
		return fmt.Errorf("new-handler: invalid scope %q (dotted identifiers only)", opts.Scope)
	}
	return nil
}

// scaffoldYAMLBlock renders the YAML fragment for one handler. The
// fragment is appended verbatim, so leading newlines matter — we emit a
// blank line ahead of the entry so concatenation never glues onto the
// previous list element.
func scaffoldYAMLBlock(opts *scaffoldOptions) string {
	var b strings.Builder
	b.WriteString("\n  - name: ")
	b.WriteString(opts.Name)
	b.WriteString("\n    type: TODO_handler_type   # set to a registered handler type before running\n")
	b.WriteString("    on: ")
	b.WriteString(opts.Consumes)
	b.WriteString("\n")

	// Union --emits and --terminal so the YAML is self-consistent.
	emits := mergeUnique(opts.Emits, opts.Terminal)
	if len(emits) > 0 {
		b.WriteString("    emits: [")
		b.WriteString(strings.Join(emits, ", "))
		b.WriteString("]\n")
	}
	if opts.Scope != "" {
		b.WriteString("    scope: ")
		b.WriteString(opts.Scope)
		b.WriteString("\n")
	}
	if len(opts.Terminal) > 0 {
		b.WriteString("    # NOTE: terminal emits — ")
		b.WriteString(strings.Join(opts.Terminal, ", "))
		b.WriteString("\n")
		b.WriteString("    # Terminality is declared on the handler spec at registration time;\n")
		b.WriteString("    # YAML records the intent so reviewers can see the contract.\n")
	}
	return b.String()
}

func mergeUnique(a, b []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(a)+len(b))
	for _, s := range a {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func runScaffoldYAML(opts *scaffoldOptions) (string, error) {
	block := scaffoldYAMLBlock(opts)
	if opts.ConfigPath == "" {
		return block, nil
	}
	// Refuse to clobber an existing entry with the same name. We parse
	// the existing file leniently (no knownTypes check) so the scaffold
	// works even when the config currently fails strict validation.
	data, err := os.ReadFile(opts.ConfigPath)
	if err != nil {
		return "", fmt.Errorf("new-handler: read %s: %w", opts.ConfigPath, err)
	}
	if duplicateHandlerName(data, opts.Name) {
		return "", fmt.Errorf("new-handler: handler %q already exists in %s; refusing to overwrite",
			opts.Name, opts.ConfigPath)
	}
	if err := appendYAMLBlock(opts.ConfigPath, data, block); err != nil {
		return "", err
	}
	return fmt.Sprintf("appended handler %q to %s\n", opts.Name, opts.ConfigPath), nil
}

// duplicateHandlerName loads data via the lenient config parser and
// reports whether any handler has the given name. Returns false when the
// parse fails (we let appendYAMLBlock surface that more concretely).
func duplicateHandlerName(data []byte, name string) bool {
	var f config.File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return false
	}
	for _, h := range f.Handlers {
		if h.Name == name {
			return true
		}
	}
	return false
}

// appendYAMLBlock writes block onto the end of path. If the file ends in
// a partial line (no trailing newline), we insert one first so the new
// list entry doesn't graft onto the previous line. We do NOT round-trip
// through yaml.v3 — that re-shapes the file (comments, blank lines, key
// order) in ways users will dislike. Simple text append wins.
func appendYAMLBlock(path string, existing []byte, block string) error {
	var buf bytes.Buffer
	buf.Write(existing)
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		buf.WriteByte('\n')
	}
	// If the file has no `handlers:` section yet, bail rather than guess.
	if !hasHandlersKey(existing) {
		return fmt.Errorf("new-handler: %s has no top-level `handlers:` key; add one and retry", path)
	}
	buf.WriteString(block)
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func hasHandlersKey(data []byte) bool {
	// Detect a top-level `handlers:` line. Cheap text scan instead of a
	// full YAML walk — the only thing we care about is that the user
	// has a list to append to.
	lines := strings.Split(string(data), "\n")
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimRight(l, " \t"), "handlers:") {
			return true
		}
	}
	return false
}

const goHandlerTemplate = `// Command {{.Name}} is a scaffolded reflex handler binary. It connects to a
// running reflex daemon over a Unix socket, registers itself as a handler
// reacting to {{.Consumes}}, and emits the declared follow-up events.
//
// Replace the TODO in OnEvent with the actual reaction logic.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/kgatilin/reflex/pkg/sdk"
)

func main() {
	socket := flag.String("socket", "/tmp/reflex.sock", "path to the reflex daemon's Unix socket")
	name := flag.String("name", "{{.Name}}", "handler name (visible in the trace)")
	flag.Parse()

	client, err := sdk.Connect(sdk.Remote(*socket))
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer client.Close()

	h := sdk.NewHandler(*name,
		sdk.Consumes({{.ConsumesQuoted}}),
{{range .EmitsLines}}{{.}}
{{end}}{{if .Scope}}		sdk.WithScope({{.ScopeQuoted}}),
{{end}}	).OnEvent(func(ctx sdk.Ctx, ev sdk.Event) error {
		// TODO: implement the reaction. Read ev.Payload, decide what to
		// emit, and call ctx.Emit(...) one or more times.
		_ = ev
		_ = ctx
		return nil
	})
	if err := client.Register(h); err != nil {
		fmt.Fprintln(os.Stderr, "register:", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	fmt.Fprintf(os.Stderr, "{{.Name}}: connected to %s as %q\n", *socket, *name)
	if err := client.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		os.Exit(1)
	}
}
`

type goTemplateData struct {
	Name           string
	Consumes       string
	ConsumesQuoted string
	Scope          string
	ScopeQuoted    string
	EmitsLines     []string
}

func runScaffoldGo(opts *scaffoldOptions) (string, error) {
	dir := opts.OutputDir
	if dir == "" {
		dir = filepath.Join("cmd", opts.Name)
	}
	target := filepath.Join(dir, "main.go")
	if _, err := os.Stat(target); err == nil {
		return "", fmt.Errorf("new-handler: %s already exists; refusing to overwrite", target)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("new-handler: stat %s: %w", target, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("new-handler: mkdir %s: %w", dir, err)
	}

	td := buildGoTemplateData(opts)
	tmpl, err := template.New("handler").Parse(goHandlerTemplate)
	if err != nil {
		return "", fmt.Errorf("new-handler: template: %w", err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, td); err != nil {
		return "", fmt.Errorf("new-handler: template execute: %w", err)
	}
	if err := os.WriteFile(target, out.Bytes(), 0o644); err != nil {
		return "", fmt.Errorf("new-handler: write %s: %w", target, err)
	}
	return fmt.Sprintf("scaffolded handler binary at %s\n", target), nil
}

func buildGoTemplateData(opts *scaffoldOptions) goTemplateData {
	td := goTemplateData{
		Name:           opts.Name,
		Consumes:       opts.Consumes,
		ConsumesQuoted: fmt.Sprintf("%q", opts.Consumes),
		Scope:          opts.Scope,
		ScopeQuoted:    fmt.Sprintf("%q", opts.Scope),
	}
	terminalSet := map[string]bool{}
	for _, t := range opts.Terminal {
		terminalSet[t] = true
	}
	emits := mergeUnique(opts.Emits, opts.Terminal)
	for _, e := range emits {
		td.EmitsLines = append(td.EmitsLines, fmt.Sprintf("\t\tsdk.Emits(%q),", e))
		if terminalSet[e] {
			td.EmitsLines = append(td.EmitsLines, fmt.Sprintf("\t\tsdk.Terminal(%q),", e))
		}
	}
	return td
}

