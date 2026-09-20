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
//go:build ignore

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	if len(os.Args) < 7 {
		fmt.Fprintln(os.Stderr, "poster: usage: poster <scanDir> <outDir> <findings-go.json|-> <go-outcomes.json|-> <findings-web.json|-> <web-outcomes.json|-> [reviewFile...]")
		os.Exit(1)
	}
	scanDir, outDir := os.Args[1], os.Args[2]
	// fixedByLoc: finding location -> was it fixed. Built by zipping each domain's record
	// file (the order that domain's fix map iterated) with that map's aggregated
	// outcomes, then MERGING the two — so a finding is marked fixed only by the fixer
	// that actually ran for it (Go findings by `autofix`, web findings by `autofix-web`).
	// A "-" arg contributes nothing (its half adds no Fixed marks).
	fixedByLoc := buildFixed(os.Args[3], os.Args[4])
	for k, v := range buildFixed(os.Args[5], os.Args[6]) {
		fixedByLoc[k] = v
	}
	reviewFiles := os.Args[7:]

	// The scan comment: one block, three tables (or a clean bill of health each).
	var b strings.Builder
	b.WriteString("## 🔍 Automated scans\n\n")
	b.WriteString(lintTable(filepath.Join(scanDir, "lint.txt")))
	b.WriteString(gosecTable(filepath.Join(scanDir, "sast.json")))
	b.WriteString(vulnTable(filepath.Join(scanDir, "sca.txt")))

	// Frontend (web/) scans, shown only when the scan-web step ran (its output files
	// exist). Keeps the comment Go-only for backend-only repos.
	if fileExists(filepath.Join(scanDir, "web-lint.txt")) || fileExists(filepath.Join(scanDir, "web-sca.json")) {
		b.WriteString("### Frontend (web/)\n\n")
		b.WriteString(webLintTable(filepath.Join(scanDir, "web-lint.txt")))
		b.WriteString(npmAuditTable(filepath.Join(scanDir, "web-sca.json")))
	}

	bodies := []string{b.String()}
	// The reviewer findings files. Each is a markdown table; inject a "Fixed?" column
	// telling whether the auto-fixer resolved that finding (matched by location).
	for _, f := range reviewFiles {
		if body := strings.TrimSpace(readFile(f)); body != "" {
			bodies = append(bodies, injectFixedColumn(body, fixedByLoc))
		}
	}

	// PR_COMMENT_AUTHOR, when set, attributes these automated comments to a bot display
	// identity via git-factory's createPullComment author override (allowlisted
	// server-side). Empty leaves authorship as the posting credential's user.
	author := os.Getenv("PR_COMMENT_AUTHOR")
	n := 0
	for _, body := range bodies {
		n++
		payloadMap := map[string]string{"body": body}
		if author != "" {
			payloadMap["author"] = author
		}
		payload, _ := json.Marshal(payloadMap)
		out := filepath.Join(outDir, fmt.Sprintf("comment-%d.json", n))
		if err := os.WriteFile(out, payload, 0o644); err != nil {
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

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// webLintTable renders the node-native frontend scanners' output (tsc --noEmit and
// Biome lint, both appended to web-lint.txt) into a table. Formats vary, so it keeps
// diagnostic-looking lines and splits location from message best-effort; capped so a
// noisy run can't produce a giant comment.
func webLintTable(p string) string {
	lines := nonEmptyLines(readFile(p))
	var diag []string
	for _, ln := range lines {
		low := strings.ToLower(ln)
		if strings.Contains(low, "error") || strings.Contains(low, "warning") ||
			strings.Contains(ln, ".ts") || strings.Contains(ln, ".tsx") ||
			strings.Contains(ln, ".js") || strings.Contains(ln, ".jsx") {
			diag = append(diag, ln)
		}
	}
	if len(diag) == 0 {
		return "#### 🧹 Lint & types (tsc + Biome)\n\nNo issues found.\n\n"
	}
	const cap = 50
	trunc := false
	if len(diag) > cap {
		diag, trunc = diag[:cap], true
	}
	var b strings.Builder
	b.WriteString("#### 🧹 Lint & types (tsc + Biome)\n\n| Location | Message |\n|---|---|\n")
	for _, ln := range diag {
		loc, msg := ln, ""
		if parts := strings.SplitN(ln, ": ", 2); len(parts) == 2 {
			loc, msg = parts[0], parts[1]
		}
		b.WriteString("| " + cell(loc) + " | " + cell(msg) + " |\n")
	}
	if trunc {
		b.WriteString("| … | (truncated) |\n")
	}
	b.WriteString("\n")
	return b.String()
}

// npmAuditTable parses `npm audit --json` (npm 7+ shape: a vulnerabilities map) into a
// table. Keys are sorted so the output is stable across runs.
func npmAuditTable(p string) string {
	var report struct {
		Vulnerabilities map[string]struct {
			Severity string          `json:"severity"`
			Via      json.RawMessage `json:"via"`
		} `json:"vulnerabilities"`
	}
	if err := json.Unmarshal([]byte(readFile(p)), &report); err != nil || len(report.Vulnerabilities) == 0 {
		return "#### 📦 Dependencies (npm audit)\n\nNo known vulnerabilities.\n\n"
	}
	names := make([]string, 0, len(report.Vulnerabilities))
	for name := range report.Vulnerabilities {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("#### 📦 Dependencies (npm audit)\n\n| Package | Severity | Advisory |\n|---|---|---|\n")
	for _, name := range names {
		v := report.Vulnerabilities[name]
		b.WriteString("| " + cell(name) + " | " + cell(v.Severity) + " | " + cell(viaSummary(v.Via)) + " |\n")
	}
	b.WriteString("\n")
	return b.String()
}

// viaSummary pulls a human advisory title from npm audit's `via` array, whose elements
// are either an advisory object {title,url,severity} or a bare package-name string.
func viaSummary(raw json.RawMessage) string {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return ""
	}
	for _, el := range arr {
		var obj struct {
			Title string `json:"title"`
		}
		if json.Unmarshal(el, &obj) == nil && obj.Title != "" {
			return obj.Title
		}
	}
	if len(arr) > 0 {
		var s string
		if json.Unmarshal(arr[0], &s) == nil && s != "" {
			return "via " + s
		}
	}
	return ""
}

// --- Fixed? column support ---

// buildFixed zips findings.json (the order the fix map iterated) with the map's
// aggregated per-fix outcomes, producing location -> fixed. Both args may be "-"
// (no data -> empty map -> no Fixed column). Parsing is tolerant of the outcome
// element shape (a bare 1, "1", or {"fixed":"1"}) — fixed iff it carries a 1.
func buildFixed(findingsPath, outcomesPath string) map[string]bool {
	m := map[string]bool{}
	if findingsPath == "-" || outcomesPath == "-" {
		return m
	}
	var findings []struct {
		Severity string `json:"severity"`
		Location string `json:"location"`
	}
	if err := json.Unmarshal([]byte(readFile(findingsPath)), &findings); err != nil {
		return m
	}
	var outcomes []json.RawMessage
	if err := json.Unmarshal([]byte(readFile(outcomesPath)), &outcomes); err != nil {
		return m
	}
	for i, f := range findings {
		if i >= len(outcomes) {
			break
		}
		s := string(outcomes[i])
		fixed := strings.Contains(s, "1") && !strings.Contains(s, `"fixed":"0"`) && s != "0" && s != `"0"`
		m[normLoc(f.Location)] = fixed
	}
	return m
}

// normLoc normalises a finding location for matching between findings.json and a
// review-table cell (which may wrap it in backticks or add parenthetical notes).
func normLoc(s string) string {
	s = strings.ReplaceAll(s, "`", "")
	if i := strings.Index(s, " ("); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(strings.ToLower(s))
}

// fileKeyOf reduces a location to its file part ("src/x.go:42" -> "src/x.go"),
// mirroring the extractor's fileOf so a per-file batched fix's key matches a row.
func fileKeyOf(loc string) string {
	s := strings.ToLower(strings.ReplaceAll(loc, "`", ""))
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, " line "); i >= 0 {
		s = s[:i]
	}
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '/' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// injectFixedColumn adds a "Fixed?" column to each markdown table in a reviewer's
// findings file: the header gets the column, the separator a "---", each data row a
// ✅/⚠️ (or — when the location isn't in the fix set) by matching the row's Location
// cell (2nd column) against fixedByLoc.
func injectFixedColumn(md string, fixedByLoc map[string]bool) string {
	if len(fixedByLoc) == 0 {
		return md
	}
	var out []string
	for _, ln := range strings.Split(md, "\n") {
		t := strings.TrimSpace(ln)
		if !strings.HasPrefix(t, "|") {
			out = append(out, ln)
			continue
		}
		cells := strings.Split(strings.Trim(t, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		low := strings.ToLower(strings.Join(cells, "|"))
		switch {
		case strings.Contains(low, "severity") && strings.Contains(low, "location"):
			out = append(out, "| "+strings.Join(cells, " | ")+" | Fixed? |")
		case strings.HasPrefix(cells[0], "---") || cells[0] == "":
			out = append(out, "|"+strings.Repeat("---|", len(cells)+1))
		default:
			mark := "—"
			if len(cells) >= 2 {
				// Exact location first, then the FILE key (a per-file batched fix
				// records just the file); line-level keys match first.
				f, ok := fixedByLoc[normLoc(cells[1])]
				if !ok {
					f, ok = fixedByLoc[fileKeyOf(cells[1])]
				}
				if ok {
					if f {
						mark = "✅ fixed"
					} else {
						mark = "⚠️ open"
					}
				}
			}
			out = append(out, "| "+strings.Join(cells, " | ")+" | "+mark+" |")
		}
	}
	return strings.Join(out, "\n")
}
