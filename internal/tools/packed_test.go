package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func bigTree() map[string]string {
	files := map[string]string{}
	line := "// a line of source long enough that the tree clears the packing threshold\n"
	for _, name := range []string{"a", "b", "c", "d"} {
		files[name+"/gen.go"] = "package " + name + "\n" + strings.Repeat(line, 400)
	}
	files["it's/note.md"] = "an apostrophe rides along\n"
	return files
}

// THE FAILURE THAT KILLED TWO-STAGE RUN 2: a 71KB tree — the best output of any
// run to that point — refused wholesale by forge's body cap on its first check.
// The packed form must land the same tree the plain form does, proven by
// RUNNING it, because the property that matters is "a shell reads this the way
// we meant".
func TestAPackedScriptLandsTheSameTree(t *testing.T) {
	files := bigTree()
	script := WriteTreeScript(files) + "\ntrue\n"
	if len(script) <= PackThreshold {
		t.Fatalf("the fixture does not clear the threshold: %d bytes", len(script))
	}
	p := Pack(script)
	if len(p) >= len(script) {
		t.Fatalf("packing did not shrink the script: %d -> %d", len(script), len(p))
	}

	dir := t.TempDir()
	cmd := exec.Command("sh", "-e")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(p)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the packed script did not run: %v\n%s", err, out)
	}

	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("%s did not land: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("%s differs after packing", rel)
		}
	}
}

// The check's exit code must survive the pipeline, or a red suite would read as
// green — the worst possible translation.
func TestAFailingCommandFailsThroughThePacking(t *testing.T) {
	script := WriteTreeScript(bigTree()) + "\nexit 7\n"

	cmd := exec.Command("sh")
	cmd.Dir = t.TempDir()
	cmd.Stdin = strings.NewReader(Pack(script))
	err := cmd.Run()
	if err == nil {
		t.Fatal("a failing command came back green through the packing")
	}
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 7 {
		t.Fatalf("the exit code did not survive: %v", err)
	}
}
