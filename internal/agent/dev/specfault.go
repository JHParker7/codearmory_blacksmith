package dev

import (
	"regexp"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/edit"
)

// This file decides ONE question: when a verification comes back red, is that
// the expected red of test-first, or a defect in the tests themselves?
//
// It matters because the developer MAY NOT EDIT TEST FILES. If the tests are at
// fault there is no implementation that satisfies them, and every turn spent
// trying is wasted. Measured on a clean run: a specification whose helpers
// called t.Cleanup with no *testing.T in scope cost the developer 23 turns —
// declaring `var t *testing.T` in production, deleting it, declaring a `t` type
// with a Cleanup method, then a testCleanup hook — against a mean of 5.4 turns
// for a successful attempt. It got there in the end, the long way, and only
// because the ticket had budget to burn.
//
// GETTING THIS WRONG IN EITHER DIRECTION IS EXPENSIVE. Blaming a healthy
// specification abandons a ticket that was nearly done; missing a broken one
// spends the whole budget proving it. The rules below are therefore split by how
// much evidence each needs: some faults are conclusive on sight, and some have
// to be established by watching the developer try.

// stripToolPrefix removes the tool label `go test` puts in front of a position
// when the package will not typecheck.
//
// THE TOOLCHAIN REPORTS THE SAME ERROR TWO WAYS. A failed build prints
//
//	./handlers_test.go:17:2: undefined: t
//
// and a failed vet load prints the identical position behind a label
//
//	vet: ./handlers_test.go:17:2: undefined: t
//
// A matcher anchored at the start of the line sees the first and not the second,
// which is exactly what happened here: every compile fault this run produced
// arrived vet-labelled, so the classification never ran at all and the developer
// was left to flail. Nothing generates this prefix but Go itself.
func stripToolPrefix(line string) string {
	l := strings.TrimSpace(line)
	for _, p := range []string{"vet: ", "build: ", "typecheck: "} {
		if rest, ok := strings.CutPrefix(l, p); ok {
			l = strings.TrimSpace(rest)
			break
		}
	}
	return strings.TrimPrefix(l, "./")
}

// goCompileError matches "path/file.go:12:34: message", which is how the Go
// toolchain reports a compile error.
//
// THE COLUMN IS REQUIRED. A test FAILURE logs "file_test.go:96:" with no column,
// and those are ordinary red tests rather than a broken specification — matching
// them would blame the author for every failing assertion.
var goCompileError = regexp.MustCompile(`^([\w./-]+\.go):\d+:\d+: `)

// CompileErrorFiles names the files with compile errors, and says whether every
// one of them is a test file.
//
// ALL OF THEM, not any: a compile error in the developer's own file is the
// developer's to fix, and while one stands the test files cannot be judged —
// Go reports a type error at the use site, which under test-first is a test
// file, even when the declaration at fault is the implementation's.
func CompileErrorFiles(out string) (files []string, allTests bool) {
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		m := goCompileError.FindStringSubmatch(stripToolPrefix(line))
		if m == nil || seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		files = append(files, m[1])
	}
	if len(files) == 0 {
		return nil, false
	}
	for _, f := range files {
		if !edit.IsTestFile(f) {
			return files, false
		}
	}
	return files, true
}

// intrinsicTestFaults are compile errors whose cause is entirely inside the file
// that reports them. NO implementation change can affect one, so they need no
// corroboration — they are conclusive on sight.
//
// The distinction from the rest is the whole reason the streak counters exist.
// "undefined: NewStore" is the expected red of every test-first ticket and is
// cleared by writing the code. "declared and not used" is a local fact about
// that file's own text; waiting for the developer to prove it by flailing spends
// an entire attempt on a verdict visible in the first test run.
var intrinsicTestFaults = []string{
	"declared and not used",
	"no new variables on left side of :=",
	"imported and not used",
	"syntax error",
	"missing return",
	"unexpected newline",
	"non-declaration statement outside function body",
}

// IntrinsicTestErrors returns the test-file compile errors no implementation
// change could fix.
func IntrinsicTestErrors(out string) []string {
	var found []string
	for _, line := range strings.Split(out, "\n") {
		l := stripToolPrefix(line)
		m := goCompileError.FindStringSubmatch(l)
		if m == nil || !edit.IsTestFile(m[1]) {
			continue
		}
		msg := strings.ToLower(l[len(m[0]):])
		for _, p := range intrinsicTestFaults {
			if strings.Contains(msg, p) {
				found = append(found, l)
				break
			}
		}
	}
	return found
}

// stdlibPackageNames are identifiers a Go file refers to a standard library
// package by. A closed set, like the reason codes: anything unlisted is treated
// as an ordinary symbol, which is the safe direction — it costs a turn, where
// the opposite costs the whole attempt.
//
// THEY EXIST BECAUSE A MISSING IMPORT LOOKS EXACTLY LIKE A MISSING SYMBOL. Go
// reports both as "undefined: X", and for an unimported package X is the bare
// package name with no dot to give it away. A test calling url.QueryEscape
// without importing net/url yields "undefined: url", which reads as a symbol the
// implementation has yet to write — so the developer tries to declare one, which
// cannot work, and rewrites the same file until its turns run out.
var stdlibPackageNames = map[string]bool{
	"bufio": true, "bytes": true, "context": true, "crypto": true, "csv": true,
	"encoding": true, "errors": true, "exec": true, "filepath": true, "fmt": true,
	"hex": true, "html": true, "http": true, "httptest": true, "io": true,
	"json": true, "log": true, "math": true, "net": true, "os": true, "path": true,
	"rand": true, "reflect": true, "regexp": true, "runtime": true, "sort": true,
	"strconv": true, "strings": true, "sync": true, "syscall": true, "template": true,
	"testing": true, "time": true, "url": true, "utf8": true,
}

// testHandleNames are the identifiers a test binds its testing handle to.
//
// A HELPER THAT FORGOT ITS PARAMETER LOOKS EXACTLY LIKE A MISSING SYMBOL, which
// is the same trap as the stdlib package names and cost the same way. A test
// file calling t.Cleanup from a function that takes no *testing.T reports
// "undefined: t", indistinguishable from the expected red of a symbol the
// implementation owes it.
//
// It is not that, and no implementation can make it that: only a test file can
// bind a *testing.T, and the one production route the model reaches for —
// declaring the symbol itself — needs the testing package, which the gate
// refuses in production code. So it is unsatisfiable by construction.
//
// Measured on a clean run: 23 turns, four separate attempts to conjure a `t`,
// none of which could have worked.
var testHandleNames = map[string]bool{"t": true, "b": true, "f": true, "tb": true}

// undefinedSymbol returns the symbol named by an "undefined: X" compile error,
// or "" when the line is not one.
func undefinedSymbol(line string) string {
	_, rest, ok := strings.Cut(line, "undefined: ")
	if !ok {
		return ""
	}
	// The symbol runs to the first space or the end; a dotted form names a
	// package member and is handled by the caller.
	sym, _, _ := strings.Cut(strings.TrimSpace(rest), " ")
	return strings.TrimSpace(sym)
}

// unsatisfiableUndefined reports whether an "undefined: X" is one the
// implementation could never satisfy.
func unsatisfiableUndefined(sym string, inTest bool) bool {
	if sym == "" {
		return false
	}
	// A DOTTED SYMBOL NAMES A PACKAGE MEMBER. No code in this package can define
	// sync.atomic, so a test referring to one is missing an import.
	if strings.Contains(sym, ".") {
		return true
	}
	if stdlibPackageNames[sym] {
		return true
	}
	return inTest && testHandleNames[sym]
}

// NonUndefinedCompileErrors returns the compile errors that are NOT the expected
// red of test-first.
//
// An undefined symbol is what a specification written before its implementation
// MUST produce. Everything else — a redeclaration, a type mismatch, a wrong
// argument count — is a defect in the tests. The two exceptions are undefined
// symbols nothing could define: see stdlibPackageNames and testHandleNames.
func NonUndefinedCompileErrors(out string) []string {
	var found []string
	for _, line := range strings.Split(out, "\n") {
		l := stripToolPrefix(line)
		m := goCompileError.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		msg := l[len(m[0]):]
		sym := undefinedSymbol(msg)
		if sym == "" {
			found = append(found, l) // not an undefined error at all
			continue
		}
		if unsatisfiableUndefined(sym, edit.IsTestFile(m[1])) {
			found = append(found, l)
		}
	}
	return found
}

// RedIsExpected reports whether the only thing wrong is the red a test-first
// specification is supposed to produce before its implementation exists.
func RedIsExpected(out string) bool { return len(NonUndefinedCompileErrors(out)) == 0 }

// goPackageConflict matches Go's "found packages a (a.go) and b (b.go) in DIR",
// reported per directory rather than per line — hence no position.
var goPackageConflict = regexp.MustCompile(`found packages (\w+) \(([\w.-]+\.go)\) and (\w+) \(([\w.-]+\.go)\)`)

// PackageConflictFiles names the two files that disagree about the package name,
// AND ONLY WHEN THE DEVELOPER CANNOT FIX IT.
//
// Two packages in one directory is the specification's to fix when the files
// that disagree are both tests: the name is set by a file this stage may not
// edit, so there is no edit that compiles.
//
// IT IS THE DEVELOPER'S WHENEVER ITS OWN FILE IS THE ODD ONE OUT, and this
// blamed the author for exactly that. Read off a live hand-back:
//
//	found packages main (board_test.go) and store (store.go)
//
// The tests said "main" and the developer's own store.go said "store" — a file
// it had written one turn earlier and could have corrected in one edit. The
// specification was sound, and a repair attempt was spent telling its author to
// fix a file it does not own.
//
// The rule is the one CompileErrorFiles already applies: a fault is the
// specification's only when EVERY file implicated in it is a test.
func PackageConflictFiles(out string) []string {
	m := goPackageConflict.FindStringSubmatch(out)
	if m == nil {
		return nil
	}
	files := []string{m[2], m[4]}
	for _, f := range files {
		if !edit.IsTestFile(f) {
			return nil
		}
	}
	return files
}

// goStackFrame matches a repo file position in a panic trace.
var goStackFrame = regexp.MustCompile(`([\w./-]+\.go):\d+`)

// PanicOnlyInTests names the test files in a panic whose every REPO frame is a
// test file, which makes the panic the specification's own defect.
//
// A panic is normally the implementation misbehaving under a test, which is
// exactly the developer's job — but not always. A test building a malformed
// request in its own fixture panics before any assertion runs, the tests
// COMPILE so the compile route cannot see it, and the developer correctly
// diagnoses it, may not edit the test, and tries to compensate in production
// instead.
//
// THE STACK SETTLES WHOSE FAULT IT IS. A panic the implementation could cause
// puts an implementation frame in the trace. Toolchain frames are excluded by
// their GOROOT path, which always carries a /src/ segment, so what remains is
// this module's own code.
func PanicOnlyInTests(out string) []string {
	if !strings.Contains(out, "panic:") {
		return nil
	}
	var tests []string
	seen := map[string]bool{}
	for _, m := range goStackFrame.FindAllStringSubmatch(out, -1) {
		f := m[1]
		if strings.Contains(f, "/src/") {
			continue // the toolchain's own frames
		}
		if !edit.IsTestFile(f) {
			return nil // an implementation frame: the developer's to fix
		}
		if !seen[f] {
			seen[f] = true
			tests = append(tests, f)
		}
	}
	return tests
}

// JudgeSpec folds one verification into the spec-fault verdict.
//
// ONE PLACE, so a route added later is weighed against the others rather than
// bolted on beside them. The order is the order of certainty.
//
// ONLY THE DEVELOPER JUDGES. The specification author, the coverage author and
// the spec merger all MAY edit test files, so a test-file fault is theirs to fix
// and handing it to themselves would loop.
func (s *State) JudgeSpec(out string, mode Mode) {
	if mode != ModeDevelop {
		return
	}

	switch files, allTests := CompileErrorFiles(out); {
	case allTests:
		s.SpecBroken = ""
		// The streak advances only on a verification that followed a real edit;
		// otherwise repeat verifications run it up without a single attempt.
		if s.Writes > s.SpecBrokenWrites {
			s.SpecBrokenTries++
			s.SpecBrokenWrites = s.Writes
		}
		s.RedIsExpected = RedIsExpected(out)

		// A FAULT LOCAL TO THE TEST FILE IS CONCLUSIVE ON SIGHT: no implementation
		// could change it, so there is nothing for a streak to establish. It cannot
		// fire on expected red by construction — it matches faults local to the
		// file, and "undefined:" is not one of them.
		//
		// The streak routes are gated on the red NOT being the expected one.
		// "undefined: NewStore" persists across every verification until the
		// implementation is finished, so without the gate a developer three writes
		// into a large ticket blames a specification that is perfectly sound.
		if len(IntrinsicTestErrors(out)) > 0 ||
			(!s.RedIsExpected &&
				(s.SpecBrokenTries >= MinSpecBrokenTries ||
					s.TestEditRefusals >= MaxTestEditRefusals)) {
			s.SpecBroken = strings.Join(files, ", ")
			s.SpecFault = "they do not compile, and the errors are inside the test " +
				"files themselves"
		}
		return

	}

	// A file that is NOT a test failed to compile, or nothing did. Either way the
	// tests are not yet to blame: Go reports a type error at the USE SITE, which
	// under test-first is a test file even when the declaration at fault is the
	// implementation's.
	//
	// No early return is needed for it. The routes below are already conservative
	// enough to decline — PanicOnlyInTests reads every position in the output, so
	// the implementation file that failed to compile is itself an implementation
	// frame and stops it. An explicit branch here was measurably equivalent, and
	// a branch nothing can distinguish is a branch nothing keeps honest.

	// A PANIC RAISED ENTIRELY INSIDE THE TESTS is conclusive for the same reason
	// a local compile fault is: no edit to the implementation can reach it.
	// Deliberately not gated on the streak or on expected red — both exist to
	// stop a healthy specification being blamed for the red of test-first, and
	// that red is a compile failure on undefined symbols, never a panic in the
	// test's own fixture.
	if panics := PanicOnlyInTests(out); len(panics) > 0 {
		s.SpecBroken = strings.Join(panics, ", ")
		s.SpecFault = "they COMPILE and then PANIC, entirely inside the test's own " +
			"setup — read the panic in the output below and fix what raises it"
		s.RedIsExpected = false
		return
	}

	// TWO PACKAGES IN ONE DIRECTORY is the specification's and nobody else's: the
	// name is set by a test file this stage may not edit, so every implementation
	// file beside it must match a declaration the developer cannot change.
	if clash := PackageConflictFiles(out); len(clash) > 0 {
		s.SpecBroken = strings.Join(clash, ", ")
		s.SpecFault = "they declare a different package from the files beside them, " +
			"so the directory holds two packages"
		s.RedIsExpected = false
		return
	}

	// Anything else means the developer is working on a different failure, and
	// the evidence for a broken specification is stale.
	s.clearSpecVerdict()
}

// clearSpecVerdict drops the verdict and everything that supports it.
//
// TOGETHER, because a partial reset is what let a runtime panic inherit a
// compile result from a failure already resolved.
func (s *State) clearSpecVerdict() {
	s.SpecBroken, s.SpecBrokenTries, s.RedIsExpected = "", 0, false
	s.SpecFault = ""
}

// FaultOrDefault is why the specification was judged unsatisfiable, and never an
// empty string.
//
// A ROUTE THAT FORGOT TO SAY WHY MUST NOT PRODUCE A BLANK. The author reads this
// sentence and acts on it; a gap where the cause belongs is how it came to be
// told "does not compile" about tests that compiled perfectly and panicked.
func (s *State) FaultOrDefault() string {
	if f := strings.TrimSpace(s.SpecFault); f != "" {
		return f
	}
	return "no implementation could satisfy them; the verification below is the evidence"
}

// NoteTestEditRefusal records the developer being told it may not edit a test,
// and concludes from it when the evidence is already there.
//
// CONCLUDED HERE rather than only at the next verification, because an agent in
// this state may never reach one: it is being refused, not working, and the
// refusal ceiling arrives first.
func (s *State) NoteTestEditRefusal(mode Mode) {
	if mode != ModeDevelop {
		return
	}
	s.TestEditRefusals++
	if s.TestEditRefusals >= MaxTestEditRefusals && !s.RedIsExpected && s.LastCompileBroke() != "" {
		s.SpecBroken = s.LastCompileBroke()
	}
}

// LastCompileBroke names the test files that failed to compile at the most
// recent verification, or "" when that verification was not test-file-only.
func (s *State) LastCompileBroke() string {
	files, allTests := CompileErrorFiles(s.LastTest)
	if !allTests {
		return ""
	}
	return strings.Join(files, ", ")
}
