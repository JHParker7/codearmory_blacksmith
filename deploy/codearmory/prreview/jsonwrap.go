// jsonwrap: reads a body text file and emits {"body": <json-encoded contents>} to
// stdout. The comment body is built from git data (commit sha + subject) written to
// a file — so it is shell-safe (never interpolated) and JSON-safe (json.Marshal
// escapes quotes/newlines/backticks). curl then POSTs the result to git-factory.
//go:build ignore

package main

import (
	"encoding/json"
	"os"
)

func main() {
	b, err := os.ReadFile(os.Args[1])
	if err != nil {
		os.Exit(1)
	}
	payload := map[string]string{"body": string(b)}
	// PR_COMMENT_AUTHOR attributes the release comment to a bot display identity via
	// git-factory's createPullComment author override (allowlisted server-side). Empty
	// leaves authorship as the posting credential's user.
	if a := os.Getenv("PR_COMMENT_AUTHOR"); a != "" {
		payload["author"] = a
	}
	enc, _ := json.Marshal(payload)
	os.Stdout.Write(enc)
}
