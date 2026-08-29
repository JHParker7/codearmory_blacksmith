package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runScript executes a generated script in a temp directory and returns the tree
// it produced.
//
// THE SCRIPT IS ACTUALLY RUN rather than inspected, because the property that
// matters is "a shell reads this the way we meant", and no amount of counting
// quotes establishes that — a correct POSIX escape contains an odd number of
// apostrophes, so the obvious assertion is wrong about correct output.
func runScript(t *testing.T, files map[string]string) map[string]string {
	t.Helper()
	dir := t.TempDir()

	cmd := exec.Command("sh", "-e")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(WriteTreeScript(files))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the script did not run: %v\n%s\n--- script ---\n%s",
			err, out, WriteTreeScript(files))
	}

	got := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		got[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("reading back the tree: %v", err)
	}
	return got
}

// The failure this guards against cost a real outage in this repository: an
// apostrophe in a body closed the shell quote early, left the script unbalanced
// and made the shell exit on a syntax error. Nothing in it ran, so the result
// came back empty with no output explaining why.
func TestAnApostropheInAFileSurvivesTheScript(t *testing.T) {
	body := "this project's own code\n"
	got := runScript(t, map[string]string{"notes.md": body})

	if got["notes.md"] != body {
		t.Fatalf("the body did not survive quoting: %q", got["notes.md"])
	}
}

func TestAPathWithAnApostropheSurvivesTheScript(t *testing.T) {
	got := runScript(t, map[string]string{"it's/a.go": "package p\n"})

	if got["it's/a.go"] != "package p\n" {
		t.Fatalf("the file did not land at its path: %#v", got)
	}
}

// A heredoc delimiter can appear in the file being written, and would truncate
// it there. printf cannot be truncated by its own payload.
func TestAFileContainingAHeredocDelimiterIsWrittenWhole(t *testing.T) {
	body := "line one\nEOF\nline three\n"
	got := runScript(t, map[string]string{"a.txt": body})

	if got["a.txt"] != body {
		t.Fatalf("the body was truncated: %q", got["a.txt"])
	}
}

func TestShellMetacharactersInABodyAreNotInterpreted(t *testing.T) {
	body := "$(touch pwned) `touch pwned2` ${HOME} \\n not a newline\n"
	got := runScript(t, map[string]string{"a.txt": body})

	if got["a.txt"] != body {
		t.Fatalf("the body was interpreted rather than written: %q", got["a.txt"])
	}
	if _, ok := got["pwned"]; ok {
		t.Fatal("a command substitution in a file body executed")
	}
	if _, ok := got["pwned2"]; ok {
		t.Fatal("a backquoted substitution in a file body executed")
	}
}

func TestNestedDirectoriesAreCreatedBeforeTheirFiles(t *testing.T) {
	got := runScript(t, map[string]string{"src/store/deep/a.go": "package store\n"})

	if got["src/store/deep/a.go"] != "package store\n" {
		t.Fatalf("the nested file was not written: %#v", got)
	}
}

func TestTheWholeTreeIsLaidDown(t *testing.T) {
	files := map[string]string{
		"go.mod":      "module x\n",
		"src/a.go":    "package a\n",
		"src/b/c.go":  "package b\n",
		"README.md":   "# x\n",
		"empty.txt":   "",
		"src/b/d.txt": "d\n",
	}
	got := runScript(t, files)

	if len(got) != len(files) {
		t.Fatalf("wrote %d files, want %d: %#v", len(got), len(files), got)
	}
	for p, want := range files {
		if got[p] != want {
			t.Errorf("%s = %q, want %q", p, got[p], want)
		}
	}
}

// An unstable script is a cache miss and an undiffable transcript for no reason.
func TestTheScriptIsStableAcrossRuns(t *testing.T) {
	files := map[string]string{"c.go": "package p\n", "a.go": "package p\n", "b.go": "package p\n"}

	first := WriteTreeScript(files)
	for i := 0; i < 8; i++ {
		if got := WriteTreeScript(files); got != first {
			t.Fatalf("the script changed between runs:\n%s\n---\n%s", first, got)
		}
	}
}
