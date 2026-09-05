// mkbody: JSON-encodes a PR create body from title/body files + refs, safely.
// It only does file I/O + json.Marshal — NEVER network — because in the forge
// sandbox Go's http client cannot reach git-factory (measured on the pr-review
// report step; curl can, Go cannot). curl does every HTTP call; this just makes
// the one payload shell can't quote correctly (a body with quotes/newlines).
package main

import (
	"encoding/json"
	"os"
)

func main() {
	// args: <title-file> <body-file> <source_ref> <target_ref> <out-file>
	if len(os.Args) != 6 {
		os.Stderr.WriteString("usage: mkbody <title-file> <body-file> <source_ref> <target_ref> <out-file>\n")
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
	b, err := json.Marshal(payload)
	if err != nil {
		os.Stderr.WriteString("marshal: " + err.Error() + "\n")
		os.Exit(1)
	}
	if err := os.WriteFile(os.Args[5], b, 0o600); err != nil {
		os.Stderr.WriteString("write: " + err.Error() + "\n")
		os.Exit(1)
	}
}
