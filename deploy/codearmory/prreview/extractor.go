// findings extractor: reads the reviewers' markdown tables (review-security.md,
// review-quality.md) and emits a JSON array of findings to <out>. Each finding
// becomes one fix-arm-c task. Run: extractor <security.md> <quality.md> <out.json>
//
// Why a Go program and not shell: the finding cells contain arbitrary prose,
// pipes and quotes; only real JSON encoding survives being fed to a workflow map
// as values_from. The table shape is fixed by the reviewer prompt:
// | Severity | Location | Issue | Recommendation |
package main

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
)

type Finding struct {
	Severity string `json:"severity"`
	Location string `json:"location"`
	Task     string `json:"task"`
}

// normLoc canonicalises a Location cell so the same file:line written two
// different ways ("`src/store.go`:42", "src/store.go line 42") collapses to one
// key. Keeps only [a-z0-9./] after dropping the word "line".
func normLoc(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "line", "")
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '/' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func main() {
	var out []Finding
	// The two reviewers (quality + security) routinely flag the SAME issue at the
	// same file:line. Without dedup, extract emits both, the fix map fans out two
	// fix-arm-c runs, and the PR gets two functionally-identical commits with the
	// same "type(scope): subject" message. Dedup by concrete location, keeping the
	// first seen (security.md is read first). A vague location (no file.line) is
	// never deduped, so distinct findings are not merged by accident.
	seen := map[string]bool{}
	// Fixed positional args: <security.md> <quality.md> <findings-go.json> <findings-web.json> <tasks-web.json>.
	for _, path := range os.Args[1:3] {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "|") {
				continue
			}
			// Split the row into cells, dropping the leading/trailing empties.
			cells := strings.Split(strings.Trim(line, "|"), "|")
			if len(cells) < 4 {
				continue
			}
			for i := range cells {
				cells[i] = strings.TrimSpace(cells[i])
			}
			sev := strings.ToLower(cells[0])
			// Skip header and separator rows; keep only real severities.
			if sev != "low" && sev != "medium" && sev != "high" && sev != "critical" {
				continue
			}
			loc, issue, rec := cells[1], cells[2], cells[3]
			key := normLoc(loc)
			if len(key) > 3 && strings.Contains(key, ".") && strings.ContainsAny(key, "0123456789") {
				if seen[key] {
					continue
				}
				seen[key] = true
			}
			task := "Fix this review finding in the code (at " + loc + "). Issue: " + issue + ". Recommendation: " + rec
			out = append(out, Finding{Severity: sev, Location: loc, Task: task})
		}
	}
	// Split findings by domain so each half routes to the right autofixer: Go
	// findings (.go / no ext) drive the `autofix` pipeline; web findings (.ts/.tsx/
	// .js/.jsx/.css/…) drive `autofix-web`. The Go fixer's guard is only_ext .go, so
	// a web finding sent there can never be fixed (and used to be falsely marked
	// "fixed" because its Go check passed trivially).
	goF, webF := []Finding{}, []Finding{}
	for _, f := range out {
		if isWeb(f.Location) {
			webF = append(webF, f)
		} else {
			goF = append(goF, f)
		}
	}
	// PER-FILE BATCHING: group each domain's findings by their source file and emit
	// ONE combined task per file, so the fix map fans out one autofix run per FILE
	// instead of one per finding. One agent sees every finding in a file together,
	// makes coherent edits (shared imports fixed once, no two runs racing the same
	// file), and it is one clone + one fixer + one auditor + one push per file — a
	// big saving on a single-slot GPU. Blast radius is bounded to a file (a bad batch
	// holds only that file's fixes), unlike sending the whole review to one agent.
	goF = groupByFile(goF)
	webF = groupByFile(webF)
	// Record files (untracked in /workspace; survive the refresh reset) — one per
	// domain, in the same order the matching fix map iterates, so the poster can zip
	// each with its map's outcomes for the "Fixed?" column. A grouped entry's Location
	// is the FILE, which the poster matches against a review row by its file part.
	writeJSON(os.Args[3], goF)
	writeJSON(os.Args[4], webF)
	// Web task STRINGS go to a file the extract step reads into output.tasks_web.
	writeJSON(os.Args[5], taskStrings(webF))
	// STDOUT is the Go map's values_from: the Go task strings.
	arr, _ := json.Marshal(taskStrings(goF))
	os.Stdout.Write(arr)
}

// fileOf returns the source file part of a Location cell — the path before the
// line number ("src/store.go:42" and "`src/store.go` line 42" both -> "src/store.go").
// Empty means the location was too vague to name a file (never batched with others).
func fileOf(loc string) string {
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

// groupByFile collapses a domain's findings into one task PER FILE (preserving
// first-seen order). A file with a single finding is passed through unchanged (its
// Location keeps the line, so the poster's line-level match still applies); a file
// with several becomes one Finding whose Location is the file and whose Task lists
// every finding for the agent to fix in one pass. Vague locations (no file) are
// never merged with each other.
func groupByFile(fs []Finding) []Finding {
	order := []string{}
	groups := map[string][]Finding{}
	for i, f := range fs {
		k := fileOf(f.Location)
		if k == "" {
			k = "\x00" + strconv.Itoa(i) // vague: its own group, never merged
		}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], f)
	}
	out := make([]Finding, 0, len(order))
	for _, k := range order {
		g := groups[k]
		if len(g) == 1 {
			out = append(out, g[0])
			continue
		}
		var b strings.Builder
		b.WriteString("Fix these " + strconv.Itoa(len(g)) + " review findings, each with the smallest change:\n")
		for i, f := range g {
			b.WriteString(strconv.Itoa(i+1) + ". " + f.Task + "\n")
		}
		out = append(out, Finding{Severity: g[0].Severity, Location: fileOf(g[0].Location), Task: b.String()})
	}
	return out
}

func writeJSON(path string, v any) {
	b, _ := json.Marshal(v)
	os.WriteFile(path, b, 0644)
}

func taskStrings(fs []Finding) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Task
	}
	return out
}

// isWeb reports whether a finding Location points at a frontend (web/) source file.
func isWeb(loc string) bool {
	l := strings.ToLower(loc)
	if strings.HasPrefix(l, "web/") || strings.Contains(l, "/web/") {
		return true
	}
	for _, ext := range []string{".tsx", ".ts", ".jsx", ".js", ".mjs", ".cjs", ".css", ".scss", ".vue", ".html", ".json"} {
		if hasExtToken(l, ext) {
			return true
		}
	}
	return false
}

// hasExtToken matches ext only at a real extension boundary — ".js" in "package.json"
// does NOT match (an alphanumeric follows), while ".json" and "app.tsx:21" do.
func hasExtToken(l, ext string) bool {
	for i := 0; ; {
		j := strings.Index(l[i:], ext)
		if j < 0 {
			return false
		}
		k := i + j + len(ext)
		if k >= len(l) || !isAlnum(l[k]) {
			return true
		}
		i = k
	}
}

func isAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}
