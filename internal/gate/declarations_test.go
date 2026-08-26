package gate

import (
	"strings"
	"testing"
)

// DECLARATIONS ARE NOT AN IMPLEMENTATION.
//
// The already-built check counts exported declarations to decide whether a
// specification's subject exists already — a real case, because sections of one
// task share a branch and whichever lands first leaves its code there for the
// rest. Since the architect began emitting shapes so the author's tests
// type-check, every ticket starts with a tree full of exported names, and
// without this exclusion each one would read as finished work.
func TestTheArchitectsDeclarationsDoNotCountAsAnImplementation(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir+"/go.mod", "module demo\n\ngo 1.25\n")
	writeFile(t, dir+"/main.go", "package main\n\nfunc main() {}\n")
	writeFile(t, dir+"/types.go", "// "+DeclarationsMarker+"\npackage main\n\n"+
		"import \"errors\"\n\nvar ErrNotFound = errors.New(\"not found\")\n\n"+
		"type Store struct{}\n\nfunc NewStore() *Store { panic(\"x\") }\n")

	// A suite that passes WITHOUT calling any stub: the exact case the marker is
	// for, since a panicking stub cannot otherwise make a test green.
	writeFile(t, dir+"/store_test.go", "package main\n\nimport \"testing\"\n\n"+
		"func TestSentinel(t *testing.T) {\n\tif ErrNotFound == nil {\n\t\tt.Error(\"nil\")\n\t}\n}\n")

	out, _ := runIn(t, dir, SpecScript("go build ./... && go test ./...", false))

	if strings.Contains(out, SpecAlreadyBuiltMarker) {
		t.Errorf("declarations were counted as an existing implementation, so a "+
			"sound specification was called finished work:\n%s", out)
	}
	if !strings.Contains(out, SpecVacuousMarker) {
		t.Errorf("a suite asserting only that a sentinel is non-nil was not called "+
			"vacuous:\n%s", out)
	}
}

// AND A REAL IMPLEMENTATION STILL COUNTS. Excluding the declarations file must
// not blind the check to code that genuinely exists — that case is why it is
// here, and it is routine when sections share a branch.
func TestARealImplementationIsStillDetected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir+"/go.mod", "module demo\n\ngo 1.25\n")
	writeFile(t, dir+"/main.go", "package main\n\nfunc main() {}\n")
	writeFile(t, dir+"/store.go", "package main\n\ntype Store struct{ n int }\n\n"+
		"func NewStore() *Store { return &Store{} }\n\n"+
		"func (s *Store) Count() int { return s.n }\n")
	writeFile(t, dir+"/store_test.go", "package main\n\nimport \"testing\"\n\n"+
		"func TestCount(t *testing.T) {\n\tif NewStore().Count() != 0 {\n\t\tt.Error(\"x\")\n\t}\n}\n")

	out, _ := runIn(t, dir, SpecScript("go build ./... && go test ./...", false))

	if !strings.Contains(out, SpecAlreadyBuiltMarker) {
		t.Errorf("a specification whose subject is already built was not "+
			"recognised as such:\n%s", out)
	}
}
