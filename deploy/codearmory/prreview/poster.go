// Command poster formats the PR-review workflow's scanner outputs into
// GitHub-flavored markdown tables and writes each comment as a JSON payload file
// ({"body": "..."}) for the workflow's shell to POST with curl. The two reviewer
// findings files are written the same way, verbatim.
//
// It formats and JSON-encodes (encoding/json handles arbitrary markdown —
// backticks, pipes, newlines — which is fragile in shell) but does NOT post:
// Go's HTTP client matches no_proxy differently from curl and cannot reach
// git-factory's in-cluster address from the forge sandbox, whereas curl (the
// path blacksmith's publish step uses) can. So Go writes the bodies and curl
// sends them.
//
// The pr-review workflow embeds this file base64-encoded and `go run`s it, so the
// source lives here for maintenance — regenerate the embedded copy with:
//
//	base64 -w0 deploy/codearmory/prreview/poster.go
//
// Args: the scan dir, then an OUTPUT dir, then the reviewer markdown files to
// post verbatim. Writes comment-1.json … in the output dir; prints each path.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "poster: usage: poster <scanDir> <outDir> [reviewFile...]")
		os.Exit(1)
	}
	scanDir, outDir := os.Args[1], os.Args[2]

	// The scan comment: one block, three tables (or a clean bill of health each).
	var b strings.Builder
	b.WriteString("## 🔍 Automated scans\n\n")
	b.WriteString(lintTable(filepath.Join(scanDir, "lint.txt")))
	b.WriteString(gosecTable(filepath.Join(scanDir, "sast.json")))
	b.WriteString(vulnTable(filepath.Join(scanDir, "sca.txt")))

	bodies := []string{b.String()}
	// The reviewer findings files, posted verbatim (they are already markdown tables).
	for _, f := range os.Args[3:] {
		if body := strings.TrimSpace(readFile(f)); body != "" {
			bodies = append(bodies, body)
		}
	}

	// PR_COMMENT_AUTHOR, when set, attributes these automated comments to a bot display
	// identity via git-factory's createPullComment author override (allowlisted
	// server-side). Empty leaves authorship as the posting credential's user.
	author := os.Getenv("PR_COMMENT_AUTHOR")
	n := 0
	for _, body := range bodies {
		n++
		payload := map[string]string{"body": body}
		if author != "" {
			payload["author"] = author
		}
		payloadBytes, _ := json.Marshal(payload)
		out := filepath.Join(outDir, fmt.Sprintf("comment-%d.json", n))
		if err := os.WriteFile(out, payloadBytes, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "poster: write", out, err)
			continue
		}
		fmt.Println(out)
	}
}

func readFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

// lintTable turns staticcheck's `file:line:col: message (CODE)` lines into a table.
func lintTable(p string) string {
	lines := nonEmptyLines(readFile(p))
	if len(lines) == 0 {
		return "### 🧹 Lint (staticcheck)\n\nNo issues found.\n\n"
	}
	var b strings.Builder
	b.WriteString("### 🧹 Lint (staticcheck)\n\n| Location | Message |\n|---|---|\n")
	for _, ln := range lines {
		loc, msg := ln, ""
		if parts := strings.SplitN(ln, ": ", 2); len(parts) == 2 {
			loc, msg = parts[0], parts[1]
		}
		b.WriteString("| " + cell(loc) + " | " + cell(msg) + " |\n")
	}
	b.WriteString("\n")
	return b.String()
}

// gosecTable parses gosec's JSON report (-fmt=json) into a table.
func gosecTable(p string) string {
	var report struct {
		Issues []struct {
			Severity, RuleID, Details, File, Line string
		} `json:"Issues"`
	}
	if err := json.Unmarshal([]byte(readFile(p)), &report); err != nil || len(report.Issues) == 0 {
		return "### 🛡️ SAST (gosec)\n\nNo issues found.\n\n"
	}
	var b strings.Builder
	b.WriteString("### 🛡️ SAST (gosec)\n\n| Severity | Location | Rule | Details |\n|---|---|---|---|\n")
	for _, is := range report.Issues {
		loc := is.File
		if is.Line != "" {
			loc += ":" + is.Line
		}
		b.WriteString("| " + cell(is.Severity) + " | " + cell(loc) + " | " + cell(is.RuleID) + " | " + cell(is.Details) + " |\n")
	}
	b.WriteString("\n")
	return b.String()
}

// vulnTable extracts govulncheck's text "Vulnerability #N: ID" blocks into a table.
func vulnTable(p string) string {
	text := readFile(p)
	type v struct{ id, summary string }
	var vulns []v
	lines := strings.Split(text, "\n")
	for i, ln := range lines {
		ln = strings.TrimSpace(ln)
		if idx := strings.Index(ln, "Vulnerability #"); idx >= 0 {
			id := strings.TrimSpace(ln[strings.Index(ln, ":")+1:])
			summary := ""
			if i+1 < len(lines) {
				summary = strings.TrimSpace(lines[i+1])
			}
			vulns = append(vulns, v{id, summary})
		}
	}
	if len(vulns) == 0 {
		return "### 📦 Dependencies (govulncheck)\n\nNo known vulnerabilities.\n\n"
	}
	var b strings.Builder
	b.WriteString("### 📦 Dependencies (govulncheck)\n\n| Vulnerability | Summary |\n|---|---|\n")
	for _, x := range vulns {
		b.WriteString("| " + cell(x.id) + " | " + cell(x.summary) + " |\n")
	}
	b.WriteString("\n")
	return b.String()
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, strings.TrimSpace(ln))
		}
	}
	return out
}

// cell makes text safe inside a markdown table cell: pipes escaped, newlines flattened.
func cell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	return strings.TrimSpace(s)
}
