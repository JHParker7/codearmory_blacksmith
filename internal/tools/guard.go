package tools

import (
	"fmt"
	"path"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/edit"
)

// A Guard reports why a path may not be written, or nil if it may.
//
// WRITING ONLY. Reading is never restricted: an agent that cannot read the tests
// it must satisfy cannot satisfy them, and every attempt to have it work blind
// produced code written against a guess at the interface.
type Guard func(path string) error

// AllowAll permits every write. The architect stage uses it, because the files
// it produces do not exist yet and no pattern can name them in advance.
func AllowAll(string) error { return nil }

// DenyAll refuses every write.
//
// For the stages whose product is a REPORT. A reviewer that can edit stops
// reviewing and starts rewriting, and then its report describes code that no
// longer exists. Written as a guard rather than by leaving write_file out of the
// tool set because both are true and they fail differently: the missing tool is
// what the model sees, and this is what happens if someone adds it back.
func DenyAll(p string) error {
	return fmt.Errorf(
		"%s cannot be written: this stage reports on the code and does not change it. Say what is "+
			"wrong and name the file and line; the stage that owns the code will fix it", p)
}

// NoTests refuses writes to Go test files.
//
// THE LOAD-BEARING RULE OF THE WHOLE PIPELINE, and the reason it is a guard
// rather than a sentence in a prompt: a message telling the model not to edit
// tests did not stop it, repeatedly, and a refusal at the write did. A developer
// that can edit the specification can always make the suite pass without making
// the code work, and that failure is invisible — the run goes green.
func NoTests(p string) error {
	if edit.IsTestFile(p) {
		return fmt.Errorf(
			"%s is a test file and the specification is not yours to change. The tests describe "+
				"what the code must do; make the code do it. If a test looks impossible to satisfy, "+
				"say so and stop rather than editing it", p)
	}
	return nil
}

// OnlyExt permits writes only to files with one of the given extensions.
//
// Written as an allowlist rather than a set of refusals because the interesting
// question at a stage boundary is what this stage PRODUCES, and an allowlist
// says exactly that. The architect writes markdown; the developer writes Go and
// the module manifest.
func OnlyExt(exts ...string) Guard {
	allowed := make(map[string]bool, len(exts))
	for _, e := range exts {
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		allowed[strings.ToLower(e)] = true
	}
	names := strings.Join(exts, ", ")
	return func(p string) error {
		if allowed[strings.ToLower(path.Ext(p))] {
			return nil
		}
		// THE MODULE MANIFEST IS ALWAYS WRITABLE ALONGSIDE .go. Measured on the
		// python rebuild: with Go source writable and go.mod not, the developer
		// could not add a `require` line, could not reach the network to fetch one
		// either, and so reimplemented uuid, chi and sqlmock out of the standard
		// library instead — running out of budget partway. An agent that cannot
		// manage its dependencies will route around not having any.
		if allowed[".go"] {
			switch path.Base(p) {
			case "go.mod", "go.sum":
				return nil
			}
		}
		return fmt.Errorf(
			"%s cannot be written at this stage, which produces %s files. Put the change in a file "+
				"this stage owns, or leave it for the stage that owns this one", p, names)
	}
}

// Both applies two guards, reporting the first refusal.
func Both(a, b Guard) Guard {
	return func(p string) error {
		if err := a(p); err != nil {
			return err
		}
		return b(p)
	}
}
