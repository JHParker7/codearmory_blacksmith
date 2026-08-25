package dev

import "testing"

// THE SPECIFICATION STAGES' OWN GATE CANNOT FAIL ON A TEST FILE THAT DOES NOT
// COMPILE, so the check has to live on this side of it.
//
// gate.SpecScript exits 0 on both of its branches for a file that will not
// build: the repair branch ends on an echo after capturing the status into $rc,
// and the authoring branch treats red as success because red is what it wants.
//
// Read off run 11, ticket e1528130: handlers_test.go called handleList as
// (w, r) at 6 sites and as (w, r, s) at 2 others. The author was told it passed
// and stopped after one edit on each of two repairs; the developer handed it
// back each time; MaxSpecRepairs ran out and the ticket blocked with 6 sites
// still wrong. The gate said yes to a file that had never compiled.
func TestAnAuthorIsNotFinishedWhileItsTestsDoNotCompile(t *testing.T) {
	const arity = "./handlers_test.go:105:17: not enough arguments in call to handleList\n" +
		"\thave (http.ResponseWriter, *http.Request)\n" +
		"\twant (http.ResponseWriter, *http.Request, *Store)"

	for name, mode := range map[string]Mode{
		"the author writing the section": ModeTest,
		"the merge reconciling sections": ModeSpecMerge,
	} {
		t.Run(name, func(t *testing.T) {
			s := &State{Staged: map[string]string{"handlers_test.go": "package main\n"}}
			// true is what the script really reports for this output.
			s.RecordVerification(arity, true, mode)

			if s.TestsPass {
				t.Error("a test file that does not compile was recorded as passing")
			}
			if s.Finished() {
				t.Error("the stage would have finished and pushed a specification " +
					"that has never compiled")
			}
		})
	}
}

// AND THE EXPECTED RED IS STILL A PASS. "undefined: NewStore" is what a
// specification written before its implementation MUST produce — failing the
// author for it would mean no test-first ticket could ever leave this stage.
//
// This is the half that makes the guard `RedIsExpected` rather than `exit $rc`.
func TestTheRedOfTestFirstStillFinishesTheAuthor(t *testing.T) {
	const expected = "./store_test.go:12:10: undefined: NewStore\n" +
		"./store_test.go:19:2: undefined: Store"

	for name, mode := range map[string]Mode{
		"the author writing the section": ModeTest,
		"the merge reconciling sections": ModeSpecMerge,
	} {
		t.Run(name, func(t *testing.T) {
			s := &State{Staged: map[string]string{"store_test.go": "package main\n"}}
			s.RecordVerification(expected, true, mode)

			if !s.TestsPass {
				t.Error("the expected red of test-first was treated as a broken spec")
			}
			if !s.Finished() {
				t.Error("an author that had done its job correctly was not allowed " +
					"to finish")
			}
		})
	}
}

// THE DEVELOPER IS NOT TOUCHED BY THIS. Its gate is a different script and it
// already fails on a compile error; re-judging its output here would be a second
// rule to keep in step with the first.
//
// THE OUTPUT HAS TO CARRY A COMPILE ERROR FOR THIS TO PIN ANYTHING. The gate
// prints an ADVISORY section quoting problems it is not failing on — run 11's
// developer gate opened with "vet: ./handlers_test.go:105:18: not enough
// arguments in call to handleList" and still exited 0. Written with clean
// output this test passed with the mode check deleted, which is no test at all.
func TestTheDeveloperKeepsWhateverItsOwnGateDecided(t *testing.T) {
	const advisory = "===ADVISORY-OPEN===\n" +
		"vet: ./handlers_test.go:105:18: not enough arguments in call to handleList\n" +
		"===ADVISORY-CLOSE===\nPASS\nok  \tdemo\t0.2s"

	s := &State{Staged: map[string]string{"store.go": "package main\n"}}
	s.RecordVerification(advisory, true, ModeDevelop)

	if !s.TestsPass || !s.Finished() {
		t.Error("the developer's passing verification was overruled by the author's " +
			"rule, on an advisory the gate had already decided not to fail")
	}
}

// A STAGE THAT AUTHORS TESTS IS THE ONE THAT OWNS WHETHER THEY COMPILE, and the
// two that do are the only two that may write a test file. If a mode is added to
// one list and not the other, one of them is wrong.
func TestOnlyTheTestWritingStagesOwnTheSpecification(t *testing.T) {
	for mode, want := range map[Mode]bool{
		ModeTest:      true,
		ModeSpecMerge: true,
		ModeDevelop:   false,
		ModeCoverage:  false,
	} {
		if got := mode.WritesSpec(); got != want {
			t.Errorf("mode %d WritesSpec = %v, want %v", mode, got, want)
		}
	}
}
