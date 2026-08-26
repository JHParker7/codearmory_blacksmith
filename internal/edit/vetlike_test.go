package edit

import (
	"errors"
	"strings"
	"testing"
)

// THE FAULT THAT COST RUN 81 A TICKET, refused at the write.
//
// A format directive in a call that does not format. The author's own gate could
// not see it: with no implementation the package does not type-check, so vet
// never ran. The developer met it later, diagnosed it correctly and repeatedly,
// and could not fix it — the fault was inside a test file. 99 turns across three
// attempts, not one verification, and the ticket blocked.
func TestAFormatDirectiveInAPlainCallIsRefused(t *testing.T) {
	src := `package main

import "testing"

func TestX(t *testing.T) {
	t.Error("NewStore returned nil for %q", "x")
}
`
	err := VetLike("store_test.go", src)
	if err == nil {
		t.Fatal("a plain Error call carrying a format directive was accepted")
	}
	if !errors.Is(err, ErrWouldNotVet) {
		t.Errorf("the refusal is not identifiable: %v", err)
	}
	for _, want := range []string{"Error", "%q", "Errorf"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// THE -f VARIANTS FORMAT, so they take directives and must not be refused.
func TestTheFormattingVariantsAreLeftAlone(t *testing.T) {
	src := `package main

import "testing"

func TestX(t *testing.T) {
	t.Errorf("got %q want %q", "a", "b")
	t.Fatalf("bad %d", 2)
	t.Logf("%v", nil)
}
`
	if err := VetLike("store_test.go", src); err != nil {
		t.Errorf("a correct formatting call was refused: %v", err)
	}
}

// AND AN ESCAPED PERCENT IS NOT A DIRECTIVE. A message that legitimately prints
// a percent sign has to stay writable.
func TestAnEscapedPercentIsNotADirective(t *testing.T) {
	src := `package main

import "testing"

func TestX(t *testing.T) {
	t.Error("the directive %%d is printed literally, not formatted")
}
`
	if err := VetLike("store_test.go", src); err != nil {
		t.Errorf("an escaped percent was treated as a directive: %v", err)
	}
}

// A SHIPPED FILE MUST NOT IMPORT A TESTING PACKAGE, and the refusal has to name
// the RENAME.
//
// Read off run 83: the developer wrote helpers into server_test_helpers.go,
// imported testing, and was told only that shipped code must not import a
// testing package. It spent 26 writes across three attempts trying to remove an
// import the file existed to use. With the import it is production code
// importing testing; without it the helpers cannot take a *testing.T. Only the
// rename works, and nothing said so.
func TestAProductionFileImportingTestingIsRefusedWithTheRename(t *testing.T) {
	src := `package main

import "testing"

func helper(t *testing.T) {}
`
	// A name the widened IsTestFile does not claim, so this is genuinely
	// production code.
	err := VetLike("helpers.go", src)
	if err == nil {
		t.Fatal("a production file importing testing was accepted")
	}
	// BOTH THE ACTION AND THE TARGET. "_test" alone could survive a refusal that
	// merely mentions test files; what run 83's developer needed was the verb.
	for _, want := range []string{"rename", "_test"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q, so it does not name the one move "+
				"that works: %v", want, err)
		}
	}
}

// AND A TEST FILE IMPORTING TESTING IS ENTIRELY NORMAL.
func TestATestFileMayImportTesting(t *testing.T) {
	src := `package main

import "testing"

func TestX(t *testing.T) {}
`
	for _, p := range []string{"store_test.go", "server_test_helpers.go"} {
		if err := VetLike(p, src); err != nil {
			t.Errorf("VetLike(%q) refused a test file for importing testing: %v", p, err)
		}
	}
}

// A FILE THAT DOES NOT PARSE IS NOT THIS CHECK'S BUSINESS. The syntax check
// reports parse errors, and reporting them twice in different words helps
// nobody.
func TestAnUnparseableFileIsLeftToTheSyntaxCheck(t *testing.T) {
	if err := VetLike("store.go", "package main\n\nfunc Broken( {\n"); err != nil {
		t.Errorf("a parse error was reported as a vet finding: %v", err)
	}
}
