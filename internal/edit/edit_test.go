package edit

import (
	"strings"
	"testing"
)

const sample = `package store

import "sync"

type Store struct {
	mu    sync.Mutex
	tasks map[string]Task
}

func (s *Store) Add(t Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[t.ID] = t
}

func main() {
	s := &Store{}
	_ = s
}
`

func TestTextAddressingResolvesAUniqueQuote(t *testing.T) {
	from, to, err := ResolveText(sample, "func main() {")
	if err != nil {
		t.Fatalf("ResolveText: %v", err)
	}
	if from != to {
		t.Errorf("a one-line quote resolved to %d-%d", from, to)
	}
	if got := split(sample)[from-1]; got != "func main() {" {
		t.Errorf("resolved to line %d, which is %q", from, got)
	}
}

func TestTextAddressingResolvesAMultiLineQuote(t *testing.T) {
	from, to, err := ResolveText(sample, "\ts.mu.Lock()\n\tdefer s.mu.Unlock()")
	if err != nil {
		t.Fatalf("ResolveText: %v", err)
	}
	if to-from != 1 {
		t.Errorf("a two-line quote resolved to %d-%d", from, to)
	}
}

// AMBIGUITY IS TEXT ADDRESSING'S ARITHMETIC. A quote matching four places does
// not say which one is meant, and the candidates have to be named or the agent
// cannot widen it.
func TestAnAmbiguousQuoteIsRefusedWithItsCandidates(t *testing.T) {
	src := "a := 1\nb := 2\na := 1\nc := 3\na := 1\n"
	_, _, err := ResolveText(src, "a := 1")
	if err == nil {
		t.Fatal("a quote appearing three times was accepted")
	}
	if !strings.Contains(err.Error(), "3 times") {
		t.Errorf("err = %v, want the count", err)
	}
	// Named by line, so the agent can pick one with a range instead.
	for _, line := range []string{"1", "3", "5"} {
		if !strings.Contains(err.Error(), line) {
			t.Errorf("err = %v, want line %s among the candidates", err, line)
		}
	}
}

// WHERE IT DIVERGED, NOT MERELY THAT IT DID. The model quotes long spans from
// memory and one wrong character fails the whole match; "does not appear" leaves
// it to guess which character.
func TestAQuoteThatNearlyMatchesSaysWhereItStopped(t *testing.T) {
	old := "func (s *Store) Add(t Task) {\n\ts.mu.Lock()\n\ts.tasks[t.ID] = t\n}"
	_, _, err := ResolveText(sample, old)
	if err == nil {
		t.Fatal("a quote that does not match was accepted")
	}
	msg := err.Error()
	if !strings.Contains(msg, "first 2 line(s) DO match") {
		t.Errorf("err = %v, want the number of lines that agreed", err)
	}
	// Both halves of the disagreement, or the agent cannot see what to change.
	if !strings.Contains(msg, "you wrote") || !strings.Contains(msg, "the file has") {
		t.Errorf("err = %v, want both sides of the divergence", err)
	}
	if !strings.Contains(msg, "s.tasks[t.ID] = t") {
		t.Errorf("err = %v, want the line the agent wrote", err)
	}
}

func TestAQuoteWithNoAnchorAtAllSaysSo(t *testing.T) {
	_, _, err := ResolveText(sample, "func nothingLikeThis() {\n\treturn\n}")
	if err == nil {
		t.Fatal("a quote with nothing in common was accepted")
	}
	if !strings.Contains(err.Error(), "not even its first line") {
		t.Errorf("err = %v, want it to say there was no anchor", err)
	}
	// It must point at the alternative rather than being a dead end.
	if !strings.Contains(err.Error(), "decl") {
		t.Errorf("err = %v, want it to name the other addressing mode", err)
	}
}

// A RETYPED QUOTE WITH THE WRONG INDENTATION IS A TRANSCRIPTION SLIP, not a
// different intention — accepted, but ONLY when it still resolves to one place,
// so tolerance never buys ambiguity.
func TestIndentationIsForgivenOnlyWhenItStaysUnambiguous(t *testing.T) {
	t.Run("one place", func(t *testing.T) {
		from, to, err := ResolveText(sample, "s.mu.Lock()") // no leading tab
		if err != nil {
			t.Fatalf("a quote with the indentation retyped was refused: %v", err)
		}
		if from != to {
			t.Errorf("resolved to %d-%d", from, to)
		}
	})

	t.Run("several places", func(t *testing.T) {
		src := "if a {\n\tx()\n}\nif b {\n        x()\n}\n"
		_, _, err := ResolveText(src, "x()")
		if err == nil {
			t.Fatal("an indentation-insensitive match that hit two places was accepted")
		}
	})
}

func TestAnEmptyQuoteIsRefused(t *testing.T) {
	if _, _, err := ResolveText(sample, ""); err == nil {
		t.Error("an empty old_str was accepted")
	}
}

// DECLARATION ADDRESSING IS IMMUNE TO LINE DRIFT, and accepts both spellings of
// a method because a model writing prose about one writes it either way.
func TestDeclarationAddressingFindsFunctionsTypesAndMethods(t *testing.T) {
	cases := map[string]string{
		"main":         "func main() {",
		"Store":        "type Store struct {",
		"Store.Add":    "func (s *Store) Add(t Task) {",
		"(*Store).Add": "func (s *Store) Add(t Task) {",
	}
	for name, wantLine := range cases {
		from, _, err := ResolveDecl(sample, name)
		if err != nil {
			t.Errorf("ResolveDecl(%q): %v", name, err)
			continue
		}
		if got := split(sample)[from-1]; got != wantLine {
			t.Errorf("ResolveDecl(%q) resolved to %q, want %q", name, got, wantLine)
		}
	}
}

func TestDeclarationAddressingSpansTheWholeDeclaration(t *testing.T) {
	from, to, err := ResolveDecl(sample, "Store.Add")
	if err != nil {
		t.Fatalf("ResolveDecl: %v", err)
	}
	lines := split(sample)
	if !strings.HasPrefix(lines[from-1], "func (s *Store) Add") {
		t.Errorf("the span starts at %q", lines[from-1])
	}
	if strings.TrimSpace(lines[to-1]) != "}" {
		t.Errorf("the span ends at %q, want the closing brace", lines[to-1])
	}
}

// A NAME THAT IS NOT THERE MUST SAY WHAT IS. "No such declaration" alone gives
// the agent nothing to correct towards.
func TestAMissingDeclarationNamesWhatTheFileDoesDeclare(t *testing.T) {
	_, _, err := ResolveDecl(sample, "notHere")
	if err == nil {
		t.Fatal("a declaration that is not in the file was resolved")
	}
	for _, want := range []string{"notHere", "main", "Store"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
}

// A FILE THAT DOES NOT PARSE CANNOT BE SEARCHED FOR A DECLARATION, and the
// message has to say that rather than "no such declaration" — the agent usually
// broke it itself, and the recovery is different.
func TestABrokenFileSaysItCannotBeParsed(t *testing.T) {
	_, _, err := ResolveDecl("package p\nfunc main() {\n", "main")
	if err == nil {
		t.Fatal("a declaration was located in a file that does not parse")
	}
	if !strings.Contains(err.Error(), "does not parse") {
		t.Errorf("err = %v, want it to say the file is broken", err)
	}
	// And it must name the way out.
	if !strings.Contains(err.Error(), "undo_edit") && !strings.Contains(err.Error(), "start_line") {
		t.Errorf("err = %v, want a recovery to point at", err)
	}
}

// A DECLARATION THAT IS NOT THERE YET IS ONE TO ADD. Test-first work exists to
// write functions that do not exist, and the declaration address could only ever
// replace — measured at 55 refusals against a file that was supposed to gain
// exactly that function.
func TestAMissingDeclarationBecomesAnAppendWhenTheReplacementIsOne(t *testing.T) {
	span, err := ResolveOrAppend(sample, "NewStore", "func NewStore() *Store {\n\treturn &Store{}\n}")
	if err != nil {
		t.Fatalf("ResolveOrAppend: %v", err)
	}
	if !span.Append {
		t.Error("a new declaration did not become an append")
	}
}

// THE CONDITION IS THE WHOLE SAFETY OF IT. Appending a stray fragment breaks the
// file, and the agent then spends its budget repairing damage the harness did.
func TestAMissingDeclarationIsStillRefusedWhenTheReplacementIsAFragment(t *testing.T) {
	for _, replace := range []string{
		"s.tasks[t.ID] = t", // a bare statement
		"return &Store{}",   // half an expression
		"",                  // nothing at all
		"} else {",          // not parseable in any position
	} {
		span, err := ResolveOrAppend(sample, "NewStore", replace)
		if err == nil {
			t.Errorf("a fragment %q was appended as a declaration (span %+v)", replace, span)
		}
	}
}

func TestAnExistingDeclarationIsReplacedRatherThanAppended(t *testing.T) {
	span, err := ResolveOrAppend(sample, "main", "func main() {\n\tprintln(\"hi\")\n}")
	if err != nil {
		t.Fatalf("ResolveOrAppend: %v", err)
	}
	if span.Append {
		t.Error("an existing declaration was appended, which would duplicate it")
	}
	if span.From == 0 {
		t.Error("an existing declaration resolved to no span")
	}
}

// TWO PARSES, because a replacement is legitimately either a declaration or a
// body fragment. A single parse would refuse half of every agent's correct edits.
func TestAReplacementIsJudgedInBothPositions(t *testing.T) {
	valid := []string{
		"func f() {}",     // a declaration
		"type T struct{}", // another
		"x := 1",          // a statement
		"return nil",      // another
		"if a { b() }",    // a block
		"",                // nothing is not malformed
	}
	for _, r := range valid {
		if ReplacementIsMalformed(r) {
			t.Errorf("ReplacementIsMalformed(%q) = true, but it is valid somewhere", r)
		}
	}

	malformed := []string{
		"func f( {",
		"} else {",
		"type struct {{{",
	}
	for _, r := range malformed {
		if !ReplacementIsMalformed(r) {
			t.Errorf("ReplacementIsMalformed(%q) = false, but it parses nowhere", r)
		}
	}
}

// GO WOULD SAY "redeclared in this block" EVENTUALLY, from a line number in a
// file the agent has not seen. Naming the symbol here catches the damage on the
// turn it happens.
func TestDuplicateDeclarationsAreNamed(t *testing.T) {
	src := `package p

func f() {}
type T struct{}
func f() {}
var x = 1
var x = 2
func (T) m() {}
func (T) m() {}
`
	got := DuplicateDecls(src)
	want := map[string]bool{"f": true, "x": true, "T.m": true}
	if len(got) != len(want) {
		t.Fatalf("DuplicateDecls() = %v, want %v", got, want)
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("DuplicateDecls() named %q", n)
		}
	}
	// Sorted, so the message is stable between turns.
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Errorf("DuplicateDecls() = %v, want them ordered", got)
		}
	}
}

// The blank identifier may legitimately repeat, and a file that does not parse
// is the syntax gate's business — reporting it twice sends the author two
// different complaints about one mistake.
func TestDuplicateDeclarationsIgnoresBlanksAndUnparseableFiles(t *testing.T) {
	blank := "package p\nvar _ = 1\nvar _ = 2\nfunc _() {}\n"
	if got := DuplicateDecls(blank); len(got) != 0 {
		t.Errorf("DuplicateDecls() = %v on repeated blanks", got)
	}
	if got := DuplicateDecls("package p\nfunc f( {"); got != nil {
		t.Errorf("DuplicateDecls() = %v on a file that does not parse", got)
	}
}

func TestDeclCountIsConservativeAboutFragments(t *testing.T) {
	cases := map[string]int{
		"func f() {}":                  1,
		"func f() {}\ntype T struct{}": 2,
		"x := 1":                       0, // a statement is not a declaration
		"func f( {":                    0, // unparseable counts as none
		"":                             0,
	}
	for src, want := range cases {
		if got := DeclCount(src); got != want {
			t.Errorf("DeclCount(%q) = %d, want %d", src, got, want)
		}
	}
}

// DELIBERATELY NARROW: only a span that BEGINS with a declaration is converted,
// because that is the only case where the intent is unambiguous.
func TestADeclarationNameIsReadOnlyOffAClearFirstLine(t *testing.T) {
	cases := map[string]string{
		"func main() {\n\tx()\n}":                 "main",
		"func (s *Store) Add(t Task) {\n}":        "Store.Add",
		"func (s Store) Add(t Task) {\n}":         "Store.Add",
		"type Store struct {\n\tmu sync.Mutex\n}": "Store",
		"\ts.mu.Lock()":                           "", // starts mid-body
		"// a comment\nfunc main() {}":            "", // starts with prose
		"":                                        "",
		"func {":                                  "",
	}
	for src, want := range cases {
		if got := DeclNameFromSource(src); got != want {
			t.Errorf("DeclNameFromSource(%q) = %q, want %q", src, got, want)
		}
	}
}

// THE MODEL SUPPLIES THESE STRINGS, so path validation is a trust boundary
// rather than a tidiness check.
func TestPathValidationRefusesAnythingReachingOutsideTheCheckout(t *testing.T) {
	bad := []string{
		"/etc/passwd",
		"../../etc/cron.d/x",
		"..",
		"a/../../b",
		"",
		"   ",
		"with\x00null",
	}
	for _, p := range bad {
		if _, err := ValidatePaths([]string{p}, 10); err == nil {
			t.Errorf("ValidatePaths(%q) was accepted", p)
		}
	}

	good, err := ValidatePaths([]string{"src/main.go", "./a/b.go", "c.go"}, 10)
	if err != nil {
		t.Fatalf("ValidatePaths: %v", err)
	}
	if len(good) != 3 || good[1] != "a/b.go" {
		t.Errorf("ValidatePaths() = %q, want them cleaned", good)
	}
}

func TestPathValidationIsBounded(t *testing.T) {
	if _, err := ValidatePaths(nil, 10); err == nil {
		t.Error("an empty path list was accepted")
	}
	if _, err := ValidatePaths([]string{"a.go", "b.go", "c.go"}, 2); err == nil {
		t.Error("more paths than the limit were accepted")
	}
}

// A DIFF CARRYING "no newline at end of file" ON EVERY TOUCHED FILE is noise on
// every change, obscuring the change itself.
func TestContentGainsATrailingNewlineExceptWhenEmpty(t *testing.T) {
	cases := map[string]string{
		"package p":   "package p\n",
		"package p\n": "package p\n",
		"":            "", // a file deliberately emptied stays empty
		"a\n\n":       "a\n\n",
	}
	for in, want := range cases {
		if got := WithTrailingNewline(in); got != want {
			t.Errorf("WithTrailingNewline(%q) = %q, want %q", in, got, want)
		}
	}
}

// TEST FILES ARE THE AUTHOR'S, AND THE COMPILER'S RULE IS TOO NARROW FOR THAT.
//
// Read off run 83: the developer wrote helpers its tests call into
// server_test_helpers.go — which does not end in _test.go, so nothing stopped it
// — and imported testing. The harness then refused the file as shipped code
// importing a testing package, and it spent 26 writes across three attempts
// trying to remove an import the file existed to use. Neither direction works;
// only a rename does, and nothing suggested one.
func TestAFileNamedLikeATestBelongsToTheAuthor(t *testing.T) {
	for _, p := range []string{
		"store_test.go", "a/b/store_test.go", "./x_test.go",
		"server_test_helpers.go", // run 83's trap
		"test_helpers.go",
		"testdata.go",
		"test.go",
	} {
		if !IsTestFile(p) {
			t.Errorf("IsTestFile(%q) = false; the developer could create it and then "+
				"be unable to satisfy the test-imports gate", p)
		}
	}

	// MATCHED BY SEGMENT, NOT SUBSTRING. These are ordinary production names and
	// blocking them would trade one trap for another.
	for _, p := range []string{
		"store.go", "latest.go", "contest.go", "attestation.go", "protest.go",
		"a/testdata/x.go", // the directory is not the file
		"store_test.gox",
	} {
		if IsTestFile(p) {
			t.Errorf("IsTestFile(%q) = true; that is a production file the developer "+
				"must be able to write", p)
		}
	}
}

// EACH ADDRESS NAMES ONE MODE. Two at once is what the branching schema exists
// to make unrepresentable, and a caller has to be able to refuse it.
func TestAnEditNamesExactlyOneAddressingMode(t *testing.T) {
	cases := []struct {
		name string
		e    Edit
		mode string
		ok   bool
	}{
		{"text", Edit{OldStr: "x"}, "old_str", true},
		{"declaration", Edit{Decl: "main"}, "decl", true},
		{"lines", Edit{StartLine: 3, EndLine: 5}, "lines", true},
		{"whole file", Edit{}, "whole file", true},
		{"text and declaration", Edit{OldStr: "x", Decl: "main"}, "", false},
	}
	for _, c := range cases {
		mode, ok := c.e.Address()
		if ok != c.ok || mode != c.mode {
			t.Errorf("%s: Address() = (%q, %v), want (%q, %v)", c.name, mode, ok, c.mode, c.ok)
		}
	}
}

func TestApplyReplacesTheAddressedLines(t *testing.T) {
	src := "one\ntwo\nthree\nfour\n"

	got, err := Apply(src, Span{From: 2, To: 3}, "TWO AND THREE")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got != "one\nTWO AND THREE\nfour\n" {
		t.Errorf("Apply() = %q", got)
	}
}

// AN EMPTY REPLACEMENT DELETES, which is legitimate — it is why this is not
// guarded the way a whole-file blank is.
func TestAnEmptyReplacementDeletesTheLines(t *testing.T) {
	got, err := Apply("one\ntwo\nthree\n", Span{From: 2, To: 2}, "")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got != "one\nthree\n" {
		t.Errorf("Apply() = %q, want the line removed", got)
	}
}

func TestApplyAppendsAtTheEnd(t *testing.T) {
	got, err := Apply("one\ntwo\n", Span{Append: true}, "func f() {}")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.HasPrefix(got, "one\ntwo\n") {
		t.Errorf("Apply() = %q, want the original kept", got)
	}
	if !strings.HasSuffix(got, "func f() {}\n") {
		t.Errorf("Apply() = %q, want the addition at the end", got)
	}
}

// AN OFF-BY-ONE HERE SILENTLY EATS A LINE OF CODE, which is the class of damage
// the whole package is arranged to prevent — so the bounds are refused rather
// than clamped.
func TestApplyRefusesARangeThatIsNotInTheFile(t *testing.T) {
	src := "one\ntwo\nthree\n"
	cases := []struct {
		name string
		span Span
	}{
		{"before the start", Span{From: 0, To: 2}},
		{"past the end", Span{From: 2, To: 9}},
		{"start past the end", Span{From: 9, To: 9}},
		{"inverted", Span{From: 3, To: 1}},
	}
	for _, c := range cases {
		if _, err := Apply(src, c.span, "x"); err == nil {
			t.Errorf("%s: Apply(%+v) was accepted", c.name, c.span)
		}
	}
}

func TestApplyTreatsAZeroRangeAsAWholeFileWrite(t *testing.T) {
	got, err := Apply("old\n", Span{}, "brand new")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got != "brand new\n" {
		t.Errorf("Apply() = %q", got)
	}
}

// Resolving and applying must agree: a span from the resolver has to address the
// text the agent quoted, or the edit lands somewhere else entirely.
func TestResolveAndApplyAgreeOnWhatTheQuoteMeant(t *testing.T) {
	e := Edit{OldStr: "\ts.tasks[t.ID] = t", Replace: "\ts.tasks[t.ID] = t.Clone()"}
	span, err := Resolve(sample, e)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got, err := Apply(sample, span, e.Replace)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(got, "s.tasks[t.ID] = t.Clone()") {
		t.Error("the replacement did not land")
	}
	if strings.Contains(got, "s.tasks[t.ID] = t\n") {
		t.Error("the original line survived; the edit landed somewhere else")
	}
	// NOTHING ELSE MAY MOVE. What an edit does not mention, it does not touch.
	if !strings.Contains(got, "func main() {") || !strings.Contains(got, "type Store struct {") {
		t.Error("an edit to one line disturbed the rest of the file")
	}
}
