package main

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/kgatilin/reflex/pkg/plugin"
	"github.com/spf13/cobra"
)

// builtinPlugins is the registry of in-repo plugin handlers this binary can run
// as a child process (doc 29 Iteration 3: "built-in but launchable separately").
// The daemon spawns `reflexd plugin <name>` over stdio; each entry is a separate
// process. 3a ships "echo" (the smoke/test plugin); 3b adds "fs" and "pytest".
var builtinPlugins = map[string]plugin.Handler{
	"echo": echoHandler,
}

// echoHandler is the trivial reference plugin: it replies to any event with one
// echo.reply carrying the triggering subject. It exercises the whole seam end to
// end with no side effects.
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
	return &cobra.Command{
		Use:   "plugin NAME",
		Short: "run a built-in plugin as a child process (stdio)",
		Long:  "Run a built-in plugin over stdin/stdout. The daemon spawns this; stdout is the protocol channel, so the plugin logs only to stderr.",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			h, ok := builtinPlugins[name]
			if !ok {
				return fmt.Errorf("unknown plugin %q (built-in: %v)", name, pluginNames())
			}
			return plugin.Serve(name, h)
		},
	}
}

func pluginNames() []string {
	out := make([]string, 0, len(builtinPlugins))
	for k := range builtinPlugins {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
