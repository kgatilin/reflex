package handler

import (
	"encoding/json"
)

// decodeConfig copies a YAML-parsed map into a typed struct by round-tripping
// through JSON. This avoids a dependency on mapstructure for what is a very
// small amount of binding code in the PoC.
func decodeConfig(in map[string]any, out any) error {
	if in == nil {
		return nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
