package main

import (
	"strings"
	"testing"
)

// ONE AUTHOR OR ONE PER SLICE, AS A FLAG RATHER THAN A REWRITE.
//
// The per-slice split exists because a weaker model could not hold an assembled
// task — section specs took 3, 5 and 11 turns where the assembled task took 10
// once and failed at 49 and 80 twice. That ceiling moved once already, which is
// why the developer side collapsed to one per task. Whether it has moved for the
// AUTHOR is a question about the current model, so both shapes stay runnable and
// the answer is a measurement.

func planFixture() planTask {
	return planTask{
		task: subtask{Title: "In-memory ticket store"},
		sections: []subtask{
			{Title: "Create and validation", File: "store.go", Acceptance: []string{"rejects an empty title"}},
			{Title: "Fetch and update", File: "read.go", Acceptance: []string{"an unknown id is distinguishable"}},
		},
	}
}

// The default is the shape every run so far has used, so turning the flag off
// cannot silently change what a board does.
func TestOneAuthorIsOffByDefault(t *testing.T) {
	t.Setenv("AGENTS_SPEC_ONE_AUTHOR", "")
	if oneSpecAuthorPerTask() {
		t.Error("one author per task is on with no setting; every existing board would change shape")
	}
}

func TestOneAuthorIsReadFromTheEnvironment(t *testing.T) {
	t.Setenv("AGENTS_SPEC_ONE_AUTHOR", "true")
	if !oneSpecAuthorPerTask() {
		t.Error("AGENTS_SPEC_ONE_AUTHOR=true did not enable one author per task")
	}
}

// THE COMBINED BRIEF MUST CARRY EVERY SLICE. One author replacing several is
// only sound if it is asked for everything they were asked for between them —
// losing a slice here would lose it from the specification silently.
func TestTheCombinedBriefNamesEverySliceAndFile(t *testing.T) {
	pt := planFixture()
	got := wholeSpecDescription(Ticket{Description: "build a store"}, pt)

	for _, want := range []string{
		"Create and validation", "Fetch and update",
		"store_test.go", "read_test.go",
		"rejects an empty title", "an unknown id is distinguishable",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the combined brief does not mention %q", want)
		}
	}
}

// The per-slice brief tells its author the other slices are being written at the
// same time and it must not touch their files. One author IS the other slices,
// so that instruction would be false — and the real hazard becomes duplicate
// declarations across the files it writes itself.
func TestTheCombinedBriefDoesNotWarnAboutSimultaneousAuthors(t *testing.T) {
	got := wholeSpecDescription(Ticket{Description: "build a store"}, planFixture())

	if strings.Contains(got, "AT THE SAME TIME") {
		t.Error("the sole author is warned about slices being written at the same time; nothing else is writing")
	}
	if !strings.Contains(got, "may not be declared again") {
		t.Error("the sole author is not warned that its own files share one package")
	}
}
