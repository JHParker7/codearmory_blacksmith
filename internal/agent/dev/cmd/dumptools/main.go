// Command dumptools prints the tool definitions the developer loop offers, in
// the wire shape a model receives them.
//
// SO A LIVE CHECK RUNS AGAINST WHAT SHIPS. Verifying a model against a
// hand-copied schema proves the copy works, which is the mistake this repository
// keeps finding in its own fakes.
package main

import (
	"encoding/json"
	"os"

	"github.com/code-armory-app/blacksmith/internal/agent/dev"
)

func main() {
	var out []map[string]any
	for _, t := range dev.Tools(dev.ModeTest) {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
			},
		})
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		panic(err)
	}
}
