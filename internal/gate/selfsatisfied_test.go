package gate

import (
	"os"
	"strings"
	"testing"
)

// declared writes an architect declarations file with one four-method interface.
func declared(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir+"/types", 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir+"/types/types.go", "// "+DeclarationsMarker+"\npackage types\n\n"+
		"type Store interface {\n"+
		"\tCreate(title string) error\n"+
		"\tGet(id int) (string, error)\n"+
		"\tList() []string\n"+
		"\tUpdate(id int) error\n}\n")
}

// A SPECIFICATION MUST NOT IMPLEMENT ITS OWN SUBJECT.
//
// Read off run 93. types/types.go declared Store with four methods; store_test.go
// then declared inMemoryStore carrying all four, plus a constructor returning it,
// ahead of any test function. The ticket became unsatisfiable both ways — an
// implementation in store.go collides with the one in the tests and will not
// compile, and no implementation leaves the suite passing against the author's
// own code. The developer diagnosed it exactly ("it lives in store_test.go") and
// oscillated for 43 turns, because it may not edit the file holding the cause.
func TestATestFileImplementingTheContractIsRefused(t *testing.T) {
	dir := t.TempDir()
	declared(t, dir)
	writeFile(t, dir+"/store_test.go", "package main\n\n"+
		"type inMemoryStore struct{}\n\n"+
		"func (s *inMemoryStore) Create(title string) error { return nil }\n"+
		"func (s *inMemoryStore) Get(id int) (string, error) { return \"\", nil }\n"+
		"func (s *inMemoryStore) List() []string { return nil }\n"+
		"func (s *inMemoryStore) Update(id int) error { return nil }\n")

	out, ok := runIn(t, dir, SelfSatisfiedScript())

	if ok {
		t.Fatalf("a test file implementing the declared contract passed:\n%s", out)
	}
	for _, want := range []string{"store_test.go", "Store", ReasonSelfSatisfied} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, out)
		}
	}
}

// A PARTIAL STUB IS AN ORDINARY FIXTURE and must stay allowed. Standing in for a
// collaborator is how tests are written; carrying the WHOLE contract is what
// makes it the implementation.
func TestAPartialStubIsAllowed(t *testing.T) {
	dir := t.TempDir()
	declared(t, dir)
	writeFile(t, dir+"/store_test.go", "package main\n\n"+
		"type partialStore struct{}\n\n"+
		"func (p *partialStore) Create(title string) error { return nil }\n"+
		"func (p *partialStore) Get(id int) (string, error) { return \"\", nil }\n")

	if out, ok := runIn(t, dir, SelfSatisfiedScript()); !ok {
		t.Errorf("a two-of-four stub was refused as an implementation:\n%s", out)
	}
}

// AND A FIXTURE UNRELATED TO THE CONTRACT IS NOT ITS BUSINESS.
func TestAnUnrelatedFixtureIsAllowed(t *testing.T) {
	dir := t.TempDir()
	declared(t, dir)
	writeFile(t, dir+"/store_test.go", "package main\n\n"+
		"type fakeClock struct{}\n\n"+
		"func (f *fakeClock) Now() int64 { return 0 }\n")

	if out, ok := runIn(t, dir, SelfSatisfiedScript()); !ok {
		t.Errorf("an unrelated fixture was refused:\n%s", out)
	}
}

// WITH NO DECLARATIONS THERE IS NOTHING TO IMPLEMENT, and the check must not
// object to a repository that predates them.
func TestATreeWithoutDeclarationsPasses(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir+"/store_test.go", "package main\n\n"+
		"type inMemoryStore struct{}\n\n"+
		"func (s *inMemoryStore) Create(title string) error { return nil }\n")

	if out, ok := runIn(t, dir, SelfSatisfiedScript()); !ok {
		t.Errorf("a tree with no declarations was refused:\n%s", out)
	}
}

// A ONE-METHOD INTERFACE IS NOT EVIDENCE. Implementing a single method is what
// every stub does, so the check requires at least two before it means anything.
func TestASingleMethodInterfaceIsNotEnoughToJudge(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/types", 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir+"/types/types.go", "// "+DeclarationsMarker+"\npackage types\n\n"+
		"type Clock interface {\n\tNow() int64\n}\n")
	writeFile(t, dir+"/store_test.go", "package main\n\n"+
		"type fakeClock struct{}\n\n"+
		"func (f *fakeClock) Now() int64 { return 0 }\n")

	if out, ok := runIn(t, dir, SelfSatisfiedScript()); !ok {
		t.Errorf("a stub for a one-method interface was refused:\n%s", out)
	}
}

// THE DEVELOPER'S IMPLEMENTATION IS THE WHOLE POINT and must never be refused.
//
// This is the case that makes the check safe to run at all: store.go carrying
// every method of the declared interface is exactly what the ticket asks for.
// Searching anything but _test.go files would refuse the work as it is done.
func TestTheDevelopersOwnImplementationIsNotRefused(t *testing.T) {
	dir := t.TempDir()
	declared(t, dir)
	writeFile(t, dir+"/store.go", "package main\n\n"+
		"type inMemoryStore struct{}\n\n"+
		"func (s *inMemoryStore) Create(title string) error { return nil }\n"+
		"func (s *inMemoryStore) Get(id int) (string, error) { return \"\", nil }\n"+
		"func (s *inMemoryStore) List() []string { return nil }\n"+
		"func (s *inMemoryStore) Update(id int) error { return nil }\n")

	if out, ok := runIn(t, dir, SelfSatisfiedScript()); !ok {
		t.Errorf("the implementation the ticket asks for was refused:\n%s", out)
	}
}

// AND ONLY THE ARCHITECT'S CONTRACT COUNTS. An interface a test declares for its
// own use, or one in an ordinary source file, is not the thing the developer was
// told to build — so implementing it in a test is nobody's mistake.
func TestOnlyTheDeclaredContractIsProtected(t *testing.T) {
	dir := t.TempDir()
	// No blacksmith:declarations marker: an ordinary source file.
	writeFile(t, dir+"/ports.go", "package main\n\n"+
		"type Notifier interface {\n\tSend(msg string) error\n\tClose() error\n}\n")
	writeFile(t, dir+"/store_test.go", "package main\n\n"+
		"type fakeNotifier struct{}\n\n"+
		"func (f *fakeNotifier) Send(msg string) error { return nil }\n"+
		"func (f *fakeNotifier) Close() error { return nil }\n")

	if out, ok := runIn(t, dir, SelfSatisfiedScript()); !ok {
		t.Errorf("a fake for an interface the architect never declared was refused:\n%s", out)
	}
}
