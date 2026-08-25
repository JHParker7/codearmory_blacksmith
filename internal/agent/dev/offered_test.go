package dev

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/edit"
)

// AN OFFERED TOOL MUST REACH THE LOOP, not merely decode.
//
// The existing check asked whether ActionFromCall accepts each offered tool. It
// does — and the loop refused it anyway, because Advance switched on the action
// name and the flat write was not one of the cases. So the stage advertised a
// tool, parsed it, and then told the model "write_file is not an action this
// stage can take", twenty turns running, until the attempt was spent.
//
// Decoding and dispatch are two gates and passing the first says nothing about
// the second. This walks every offered tool through BOTH.
func TestEveryOfferedToolIsHandledByTheLoop(t *testing.T) {
	// Arguments that satisfy each tool, so the action reaches Advance rather than
	// failing on its own contents.
	args := map[string]string{
		ActionReadFiles: `{"paths":["main.go"]}`,
		ActionWriteFile: `{"path":"main.go","decl":"main","replace":"func main(){}",` +
			`"summary":"s","type":"fix"}`,
		ActionUndoEdit: `{}`,
	}

	for _, mode := range []Mode{ModeDevelop, ModeTest, ModeCoverage} {
		for _, tool := range Tools(mode) {
			raw, ok := args[tool.Name]
			if !ok {
				t.Fatalf("mode %d offers %q and this test has no arguments for it — "+
					"add them rather than leaving the tool unchecked", mode, tool.Name)
			}

			act, err := ActionFromCall(toolCall(tool.Name, raw), mode)
			if err != nil {
				t.Errorf("mode %d: %q was offered but would not decode: %v", mode, tool.Name, err)
				continue
			}

			s := &State{
				Staged:   map[string]string{},
				Read:     map[string]string{"main.go": "package main\n\nfunc main() {}\n"},
				Baseline: map[string]string{"main.go": "package main\n\nfunc main() {}\n"},
			}
			_, applyErr := s.Advance(act, mode)

			// The action may legitimately be refused on its CONTENTS — an undo with
			// nothing to undo, a write a mode may not make. What it must never be is
			// unknown to the loop that offered it.
			if s.Notice != "" && strings.Contains(s.Notice, "is not an action this stage can take") {
				t.Errorf("mode %d offers %q and the loop calls it unknown: %s",
					mode, tool.Name, s.Notice)
			}
			_ = applyErr
		}
	}
}

// THE FLAT WRITE ACTUALLY STAGES ITS EDIT. Reaching Advance is not enough if it
// arrives with nothing in it: the fold from flat arguments into Edits happens in
// the decoder, and a switch that dispatches correctly on an empty edit list is
// still a turn that changed nothing.
func TestTheFlatWriteStagesWhatItWasGiven(t *testing.T) {
	act, err := ActionFromCall(toolCall(ActionWriteFile,
		`{"path":"main.go","decl":"main","replace":"func main() { println(1) }",`+
			`"summary":"s","type":"fix"}`), ModeDevelop)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	s := &State{
		Staged:   map[string]string{},
		Read:     map[string]string{"main.go": "package main\n\nfunc main() {}\n"},
		Baseline: map[string]string{"main.go": "package main\n\nfunc main() {}\n"},
	}
	verify, err := s.Advance(act, ModeDevelop)
	if err != nil {
		t.Fatalf("the flat write was refused: %v (notice: %s)", err, s.Notice)
	}
	if !verify {
		t.Fatalf("the flat write changed nothing, so no verification follows: %s", s.Notice)
	}
	got, ok := s.Staged["main.go"]
	if !ok {
		t.Fatal("the flat write staged no file")
	}
	if !strings.Contains(got, "println(1)") {
		t.Errorf("the staged file does not carry the new body:\n%s", got)
	}
}

var _ = edit.Edit{}
