package main

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/kgatilin/reflex/pkg/plugin"
	"github.com/spf13/cobra"
)

// builtin is one in-repo plugin: a builder that produces its self-description
// (announced in hello) and handler from the command-line options. The builder
// shape lets a plugin take config (e.g. fs's --root) before serving.
type builtin struct {
	build func(opts pluginOpts) (plugin.Spec, plugin.Handler, error)
}

// pluginOpts carries the flags `reflexd plugin` accepts; each plugin reads what
// it needs (echo ignores them; fs reads root).
type pluginOpts struct {
	root string
}

// builtinPlugins is the registry of in-repo plugins this binary can run as a
// child process (doc 29 Iteration 3: "built-in but launchable separately"). The
// daemon spawns `reflexd plugin <name> [flags]` over stdio; each entry is a
// separate process.
var builtinPlugins = map[string]builtin{
	"echo": {build: func(pluginOpts) (plugin.Spec, plugin.Handler, error) {
		spec := plugin.Spec{Name: "echo", Events: []plugin.EventDecl{{Kind: "echo.reply", Role: plugin.RoleOut}}}
		return spec, echoHandler, nil
	}},
	"fs":     {build: func(o pluginOpts) (plugin.Spec, plugin.Handler, error) { return buildFS(o.root) }},
	"pytest": {build: func(o pluginOpts) (plugin.Spec, plugin.Handler, error) { return buildPytest(o.root) }},
}

// echoHandler is the trivial reference plugin: it replies to any event with one
// echo.reply carrying the triggering subject. It exercises the seam with no side
// effects.
func echoHandler(ev plugin.Event) ([]plugin.Emit, error) {
	payload, err := json.Marshal(map[string]string{"subject": ev.Subject})
	if err != nil {
		return nil, err
	}
	return []plugin.Emit{{Kind: "echo.reply", Payload: payload}}, nil
}

// pluginCmd runs one built-in plugin over stdin/stdout until the host closes the
// stream. It is the child-process side of the seam.
func pluginCmd() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "plugin NAME",
		Short: "run a built-in plugin as a child process (stdio)",
		Long:  "Run a built-in plugin over stdin/stdout. The daemon spawns this; stdout is the protocol channel, so the plugin logs only to stderr.",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			b, ok := builtinPlugins[name]
			if !ok {
				return fmt.Errorf("unknown plugin %q (built-in: %v)", name, pluginNames())
			}
			spec, handler, err := b.build(pluginOpts{root: root})
			if err != nil {
				return err
			}
			return plugin.Serve(spec, handler)
		},
	}
	cmd.Flags().StringVar(&root, "root", "", "workspace root the plugin is confined to (fs)")
	return cmd
}

func pluginNames() []string {
	out := make([]string, 0, len(builtinPlugins))
	for k := range builtinPlugins {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
