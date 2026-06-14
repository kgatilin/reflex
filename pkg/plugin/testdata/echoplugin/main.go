// Command echoplugin is a minimal reflex plugin used by the pkg/plugin Spawn
// test: it replies to any event with one echo.reply carrying the input payload.
package main

import "github.com/kgatilin/reflex/pkg/plugin"

func main() {
	_ = plugin.Serve("echo", func(ev plugin.Event) ([]plugin.Emit, error) {
		return []plugin.Emit{{Kind: "echo.reply", Payload: ev.Payload}}, nil
	})
}
