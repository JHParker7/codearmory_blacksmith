package main

import (
	"strings"
	"testing"
)

// The design is model output being written into the branch every other agent
// clones, so the path rules are a boundary rather than tidying.
func TestSanitiseDesignRefusesAnythingThatIsNotDocumentation(t *testing.T) {
	d := architectDesign{Files: []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}{
		{"README.md", "# real"},
		{"main.go", "package main"},           // source, not documentation
		{"app.py", "print(1)"},                // any language, same answer
		{"index.ts", "export {}"},             // ditto
		{"build.sh", "#!/bin/sh"},             // ditto
		{"Makefile", "all:"},                  // ditto
		{"../../etc/notes.md", "escape"},      // climbs out of the tree
		{"/etc/notes.md", "absolute"},         // absolute
		{".hidden/notes.md", "dotfile"},       // hidden
		{"empty.md", "   "},                   // nothing in it
		{"ARCHITECTURE.md", "# architecture"}, // real
	}}

	files, rejected := sanitiseDesign(d)

	got := strings.Join(pathsOf(files), ",")
	if got != "README.md,ARCHITECTURE.md" {
		t.Fatalf("kept %q, want only the two markdown files", got)
	}
	for _, bad := range []string{
		"main.go", "app.py", "index.ts", "build.sh", "Makefile",
		"../../etc/notes.md", "/etc/notes.md", ".hidden/notes.md", "empty.md",
	} {
		if !strings.Contains(strings.Join(rejected, "|"), bad) {
			t.Errorf("%q was dropped without being reported; a silent drop reads as a design that simply said less", bad)
		}
	}
}

func TestSanitiseDesignBoundsSizeAndCount(t *testing.T) {
	type f = struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	big := strings.Repeat("x", maxDesignFileBytes+1)
	d := architectDesign{Files: []f{{"huge.md", big}}}
	if files, rejected := sanitiseDesign(d); len(files) != 0 || len(rejected) != 1 {
		t.Errorf("oversized file = %d kept / %d rejected, want 0/1", len(files), len(rejected))
	}

	var many []f
	for range maxDesignFiles + 3 {
		many = append(many, f{"a.md", "x"})
	}
	// Same path repeatedly: deduped rather than written N times.
	if files, _ := sanitiseDesign(architectDesign{Files: many}); len(files) != 1 {
		t.Errorf("duplicate paths = %d files, want 1", len(files))
	}

	many = nil
	for i := range maxDesignFiles + 3 {
		many = append(many, f{string(rune('a'+i)) + ".md", "x"})
	}
	if files, _ := sanitiseDesign(architectDesign{Files: many}); len(files) != maxDesignFiles {
		t.Errorf("kept %d files, want the cap of %d", len(files), maxDesignFiles)
	}
}

// This stage pushes to the branch every other agent clones. A force-push here
// would destroy work invisibly and then appear in every sandbox cut afterwards.
func TestCommitScriptRebasesAndNeverForcePushes(t *testing.T) {
	a := NewArchitectAgent(nil, nil, ClassLarge, RepoConfig{URL: "git://x/y.git", Branch: "dev"})
	s := a.commitScript([]designFile{{Path: "README.md", Content: "# hi"}}, Ticket{Title: "a request"})

	if strings.Contains(s, "--force") || strings.Contains(s, " -f ") {
		t.Fatalf("the design push force-pushes to the base branch:\n%s", s)
	}
	// RunSandbox hands the script an empty writable directory, so the clone is
	// the script's job. Without it the first `git config` failed with "fatal: not
	// in a git directory" and threw away a design the model had got right.
	clone := strings.Index(s, "git clone")
	cfg := strings.Index(s, "git config")
	if clone == -1 {
		t.Fatalf("the script never clones the repository:\n%s", s)
	}
	if cfg != -1 && cfg < clone {
		t.Errorf("the script runs git config at %d before cloning at %d; there is no repository yet", cfg, clone)
	}
	if !strings.Contains(s, "cd repo") {
		t.Errorf("the script does not enter the clone:\n%s", s)
	}
	fetch := strings.Index(s, "git fetch")
	rebase := strings.Index(s, "git rebase")
	push := strings.Index(s, "git push")
	if fetch == -1 || rebase == -1 || push == -1 {
		t.Fatalf("script must fetch, rebase and push:\n%s", s)
	}
	if !(fetch < rebase && rebase < push) {
		t.Errorf("order was fetch=%d rebase=%d push=%d, want fetch before rebase before push", fetch, rebase, push)
	}
	if !strings.Contains(s, "HEAD:'dev'") {
		t.Errorf("script does not push to the configured base branch:\n%s", s)
	}
	// An unchanged design must not become an empty commit on the shared branch.
	if !strings.Contains(s, "git diff --cached --quiet") || !strings.Contains(s, designNoChangeMarker) {
		t.Errorf("script does not guard against committing nothing:\n%s", s)
	}
}

// A design already on the branch must not be written a second time, and a
// re-poll of the same column must not re-run the stage.
func TestArchitectSkipsAnAlreadyDesignedRequest(t *testing.T) {
	a := NewArchitectAgent(nil, nil, ClassLarge, RepoConfig{URL: "git://x/y.git"})
	if !a.Wants(Ticket{}) {
		t.Error("a fresh request should be designed")
	}
	if a.Wants(Ticket{Comments: []Comment{{Body: designMarker + "\n\nsomething"}}}) {
		t.Error("a request carrying the design marker was designed twice")
	}
}

// The product manager must keep taking from the inbox on a host that runs no
// architect, or every request strands there with nothing to collect it.
func TestArchitectSettingMovesTheProductManagersQueue(t *testing.T) {
	// Restore the STATIC shape (architect off) — `stages` is package-level, so
	// leaving it enabled moves the product manager off the inbox for every test
	// that runs afterwards, and most of them start a ticket there.
	t.Cleanup(func() { applyArchitectSetting(false) })

	applyArchitectSetting(true)
	if got := stages[roleScoping].Ready; got != ColReadyForScoping {
		t.Errorf("with an architect the PM takes from %q, want %q", got, ColReadyForScoping)
	}
	if _, ok := stages[roleArchitect]; !ok {
		t.Error("the architect is enabled but absent from the routing table")
	}

	applyArchitectSetting(false)
	if got := stages[roleScoping].Ready; got != ColInbox {
		t.Errorf("without an architect the PM takes from %q, want %q", got, ColInbox)
	}
	// Absent, not merely inert: an entry still claiming the inbox would make
	// "which stage works this column" depend on map iteration order.
	if _, ok := stages[roleArchitect]; ok {
		t.Error("the architect is disabled but still in the routing table, claiming the inbox")
	}
}

// Documentation is context, not a gate: a request the architect could not design
// is still a request worth building, so exhaustion carries it FORWARD.
func TestArchitectExhaustionAdvancesRatherThanBlocks(t *testing.T) {
	t.Cleanup(func() { applyArchitectSetting(false) })
	applyArchitectSetting(true)

	s, ok := stageFor(roleArchitect)
	if !ok {
		t.Fatal("the architect has no routing entry once enabled")
	}
	if s.Exhausted != ColReadyForScoping {
		t.Errorf("exhausted goes to %q, want %q: a request nobody documented still needs breaking down",
			s.Exhausted, ColReadyForScoping)
	}
	if s.Ready != ColInbox || s.Working != ColDesigning || s.Success != ColReadyForScoping {
		t.Errorf("architect stage = %+v, want inbox → designing → ready_for_scoping", s)
	}
}

// Every column a stage names must exist on the board, or a ticket moves into a
// status the tickets service rejects.
func TestNewColumnsAreProvisioned(t *testing.T) {
	have := map[string]bool{}
	for _, c := range workflowColumns {
		have[c.Value] = true
	}
	for _, col := range []string{ColDesigning, ColReadyForScoping} {
		if !have[col] {
			t.Errorf("column %q is used by the routing table but never provisioned", col)
		}
	}
}

// THE FAILURE THAT MOTIVATED THE FENCE RULE. The architect asks for markdown
// file contents, and a README with a ```bash build snippet in it is the norm
// rather than an edge case. extractFenced takes the FIRST ``` in the reply, so
// running it over a well-formed object returned the shell snippet and threw the
// object away — reported as "invalid character ']' after top-level value" on a
// reply that was in fact perfectly valid.
func TestDesignSurvivesCodeFencesInsideTheMarkdown(t *testing.T) {
	reply := `{
  "overview": "a tracker",
  "files": [
    {"path": "README.md", "content": "# Tracker\n\n## Build\n\n` + "```" + `bash\ngo build ./...\n` + "```" + `\n\nDone.\n"},
    {"path": "ARCHITECTURE.md", "content": "# Design\n\nA store and a server.\n"}
  ]
}`
	var d architectDesign
	if err := decodeJSONObject(reply, &d); err != nil {
		t.Fatalf("decodeJSONObject on a valid object containing markdown fences: %v", err)
	}
	files, rejected := sanitiseDesign(d)
	if len(files) != 2 {
		t.Fatalf("kept %d files (rejected %v), want both", len(files), rejected)
	}
	if !strings.Contains(files[0].Content, "go build ./...") {
		t.Errorf("the fenced snippet was lost from the README:\n%s", files[0].Content)
	}
}

// The unwrapping extractFenced exists for must still work: a model that writes
// prose and then a fenced JSON block is the case it was written for.
func TestFencedJSONIsStillUnwrapped(t *testing.T) {
	reply := "Here is the design:\n\n```json\n{\"overview\":\"x\",\"files\":[{\"path\":\"README.md\",\"content\":\"# x\"}]}\n```"
	var d architectDesign
	if err := decodeJSONObject(reply, &d); err != nil {
		t.Fatalf("a fenced object must still decode: %v", err)
	}
	if d.Overview != "x" {
		t.Errorf("overview = %q, want %q", d.Overview, "x")
	}
}

// The architect must be able to READ the code, or it cannot describe what is
// really there — and on a repository that already has documentation it would
// replace it rather than update it, because the model writes whole files.
func TestContextScriptReadsDocsAndSource(t *testing.T) {
	a := NewArchitectAgent(nil, nil, ClassLarge, RepoConfig{URL: "git://x/y.git", Branch: "dev"})
	s := a.contextScript()

	if !strings.Contains(s, "git clone") || !strings.Contains(s, "cd repo") {
		t.Fatalf("the context script does not clone the repository:\n%s", s)
	}
	if !strings.Contains(s, "git ls-files") {
		t.Errorf("the context script does not list the tree:\n%s", s)
	}
	// Documentation before source: clip() cuts from the END, so whatever must
	// survive the budget has to be printed first.
	docs := strings.Index(s, "=== DOCUMENTATION ===")
	src := strings.Index(s, "=== SOURCE ===")
	if docs == -1 || src == -1 {
		t.Fatalf("the context script reads either docs or source but not both:\n%s", s)
	}
	if docs > src {
		t.Errorf("source is read before documentation; the budget would drop the docs first")
	}
	// It reads code, which is the whole point of this change.
	for _, ext := range []string{"go", "py", "ts"} {
		if !strings.Contains(s, ext) {
			t.Errorf("the context script never reads .%s files:\n%s", ext, s)
		}
	}
	// The preamble sets -e, and an empty baseline has no markdown and no source.
	// Without the guards the whole stage would abort on a new repository.
	if !strings.Contains(s, "|| true") {
		t.Errorf("an empty repository would abort the script under set -e:\n%s", s)
	}
}

// THE MIRROR OF TestDesignSurvivesCodeFencesInsideTheMarkdown, and the case that
// version missed: the model wraps its whole answer in ```json AND the content
// inside contains fences of its own.
//
// Measured live on qwen2.5-coder-14b, which fences its replies where the MoE
// emitted them bare — so this harness bug was invisible on one model and fatal
// on the other. It would have been scored as the dense model being worse at
// structured output, which it is not.
func TestFencedDesignWhoseContentAlsoContainsFences(t *testing.T) {
	reply := "```json\n" + `{
  "overview": "a tracker",
  "files": [
    {"path": "README.md", "content": "# tracker\n\n## Building\n\n` + "```" + `bash\ngo build ./...\n` + "```" + `\n\n## Running\n\n` + "```" + `bash\n./tracker\n` + "```" + `\n"},
    {"path": "ARCHITECTURE.md", "content": "# Design\n\nA store and a server.\n"}
  ]
}` + "\n```"

	var d architectDesign
	if err := decodeJSONObject(reply, &d); err != nil {
		t.Fatalf("a fenced object whose content contains fences was rejected: %v", err)
	}
	files, rejected := sanitiseDesign(d)
	if len(files) != 2 {
		t.Fatalf("kept %d files (rejected %v), want both", len(files), rejected)
	}
	// BOTH inner blocks must survive: cutting at the FIRST inner fence is exactly
	// the bug, and it would still leave a plausible-looking partial file.
	if !strings.Contains(files[0].Content, "go build ./...") || !strings.Contains(files[0].Content, "./tracker") {
		t.Errorf("the payload was cut at an inner fence:\n%s", files[0].Content)
	}
	if d.Overview != "a tracker" {
		t.Errorf("overview = %q", d.Overview)
	}
}
