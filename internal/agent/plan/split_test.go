package plan

import (
	"strings"
	"testing"
)

// A SPLIT THAT LOSES A CRITERION LOSES IT PERMANENTLY: the children replace the
// parent, so a requirement in none of them is absent everywhere and the run
// reports success. This is the r80 failure one level down from where it was
// measured.
func TestASplitMustStillAskForEverythingTheUnitAskedFor(t *testing.T) {
	parent := Subtask{Title: "The REST API", Acceptance: []string{
		"POST /tasks stores one",
		"GET /tasks returns them",
		"an oversized title is rejected",
		"a body that is not an object is rejected",
	}}

	t.Run("a faithful division is accepted", func(t *testing.T) {
		if !SplitCovers(parent, []Subtask{
			{Title: "Writes", Acceptance: []string{
				"POST /tasks stores one", "an oversized title is rejected"}},
			{Title: "Reads", Acceptance: []string{
				"GET /tasks returns them", "a body that is not an object is rejected"}},
		}) {
			t.Error("a split that divides every criterion was refused")
		}
	})

	t.Run("a dropped criterion is refused", func(t *testing.T) {
		if SplitCovers(parent, []Subtask{
			{Title: "Writes", Acceptance: []string{"POST /tasks stores one"}},
			{Title: "Reads", Acceptance: []string{"GET /tasks returns them"}},
		}) {
			t.Error("a split that silently dropped two requirements was accepted")
		}
	})

	// THE OTHER HALF OF THE SAME FAILURE. The developer is bound to whatever the
	// child says, so a paraphrase that narrows a requirement is a requirement
	// quietly changed — and this stage cannot tell a harmless rewording from a
	// narrowing one, so it takes neither.
	t.Run("a reworded criterion is refused", func(t *testing.T) {
		if SplitCovers(parent, []Subtask{
			{Title: "Writes", Acceptance: []string{
				"POST /tasks stores one", "an oversized title is rejected"}},
			{Title: "Reads", Acceptance: []string{
				"GET /tasks returns them", "bad bodies are handled"}},
		}) {
			t.Error("a split that rewrote a requirement was accepted")
		}
	})

	// WHAT IS FOLDED is only what cannot change meaning.
	t.Run("case and trailing punctuation do not count as a change", func(t *testing.T) {
		if !SplitCovers(
			Subtask{Acceptance: []string{"POST /tasks stores one."}},
			[]Subtask{{Acceptance: []string{"  post /tasks stores one  "}}},
		) {
			t.Error("a criterion was rejected over its capitalisation")
		}
	})

	t.Run("a unit with no criteria is trivially covered", func(t *testing.T) {
		if !SplitCovers(Subtask{Title: "x"}, []Subtask{{Title: "a"}, {Title: "b"}}) {
			t.Error("a unit with nothing to divide was refused")
		}
	})

	// An empty criterion carries no requirement, so it cannot be the thing that
	// refuses a split.
	t.Run("an empty criterion is not a requirement", func(t *testing.T) {
		if !SplitCovers(
			Subtask{Acceptance: []string{"one", "   "}},
			[]Subtask{{Acceptance: []string{"one"}}},
		) {
			t.Error("a blank criterion refused an otherwise faithful split")
		}
	})
}

// THE TEST FILE'S NAME IS DERIVED IN ONE PLACE, because the author is told to
// write this exact name and the developer is told it may not touch it. Two
// stages computing it separately is how they come to disagree.
func TestTheTestFileIsNamedFromItsSource(t *testing.T) {
	for in, want := range map[string]string{
		"store.go":          "store_test.go",
		"internal/store.go": "internal/store_test.go",
		"store":             "store_test.go",
		"handlers.read.go":  "handlers.read_test.go",
		"store_test.go":     "store_test_test.go",
	} {
		if got := SpecFileFor(in); got != want {
			t.Errorf("SpecFileFor(%q) = %q, want %q", in, got, want)
		}
	}
}

// THE INTRO IS A CONSTANT BECAUSE TWO STAGES DEPEND ON THE EXACT WORDS: one
// writes the sentence and the other cuts on it. If it ever stops ending at the
// opening backtick, the reader silently returns the rest of the description.
func TestTheFileIntroIsShapedForTheReaderThatCutsOnIt(t *testing.T) {
	if !strings.HasSuffix(TaskFileIntro, "`") {
		t.Errorf("TaskFileIntro = %q; the reader cuts to the next backtick", TaskFileIntro)
	}
	if strings.Count(TaskFileIntro, "`") != 1 {
		t.Errorf("TaskFileIntro = %q; a second backtick would end the name early", TaskFileIntro)
	}
}
