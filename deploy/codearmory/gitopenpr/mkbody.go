// mkbody: JSON-encodes a git-factory createPull body from title/body files + refs +
// an optional author override, safely. It only does file I/O + json.Marshal — NEVER
// network — because in the forge sandbox Go's http client cannot reach git-factory
// (measured on the pr-review report step; curl can, Go cannot). curl does every HTTP
// call; this just makes the one payload shell can't quote correctly (a body with
// quotes/newlines). An empty author is omitted so git-factory keeps its default.
package main

import (
	"encoding/json"
	"os"
)

func main() {
	// args: <title-file> <body-file> <source_ref> <target_ref> <author> <out-file>
	if len(os.Args) != 7 {
		os.Stderr.WriteString("usage: mkbody <title-file> <body-file> <source_ref> <target_ref> <author> <out-file>\n")
		os.Exit(2)
	}
	title, _ := os.ReadFile(os.Args[1])
	body, _ := os.ReadFile(os.Args[2])
	payload := map[string]string{
		"title":      string(title),
		"body":       string(body),
		"source_ref": os.Args[3],
		"target_ref": os.Args[4],
	}
	if a := os.Args[5]; a != "" {
		payload["author"] = a // attribute to the automation bot (allowlisted server-side)
	}
	b, err := json.Marshal(payload)
	if err != nil {
		os.Stderr.WriteString("marshal: " + err.Error() + "\n")
		os.Exit(1)
	}
	if err := os.WriteFile(os.Args[6], b, 0o600); err != nil {
		os.Stderr.WriteString("write: " + err.Error() + "\n")
		os.Exit(1)
	}
}
