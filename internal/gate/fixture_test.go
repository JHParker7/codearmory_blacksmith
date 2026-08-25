package gate_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/gate"
)

// runGate executes the fixture-literal check against a throwaway tree, because
// the check is a shell script and testing it any other way tests a copy of it.
func runGate(t *testing.T, files map[string]string) (string, bool) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.Command("sh", "-c", gate.FixtureLiteralScript())
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

const gamedMain = `package main

import "net/http"

func buildMux(s *Store) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", boardHandler(s))
	mux.HandleFunc("/tickets", ticketsHandler(s))
	mux.HandleFunc("/nonexistent", notFoundHandler)
	mux.HandleFunc("/api/unknown", notFoundHandler)
	return mux
}
`

const gamedTest = `package main

func TestMainUnknownPathReturns404(t *testing.T) {
	for _, p := range []string{"/nonexistent", "/api/unknown"} {
		_ = p
	}
}
`

// THE CASE THAT SHIPPED. Merged to v0.1.0 having passed every gate: 52 tests
// green, go vet clean, the reviewer called the branch clean and praised its
// thorough tests — while GET /anything-else still returned the board with 200,
// the exact thing the test was written to prevent.
func TestTheGateCatchesTheLiteralsThatShipped(t *testing.T) {
	out, ok := runGate(t, map[string]string{
		"main.go":         gamedMain,
		"main_test.go":    gamedTest,
		"ARCHITECTURE.md": "The API serves /tickets and the board at /.\n",
	})

	if ok {
		t.Fatalf("the gate passed an implementation that names the test's own inputs:\n%s", out)
	}
	for _, want := range []string{"/nonexistent", "/api/unknown", gate.ReasonTestLiteral} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, out)
		}
	}
	// AND IT SAYS WHAT TO DO INSTEAD. A refusal naming the symptom sends a
	// correct agent to the wrong place.
	if !strings.Contains(out, "match the pattern, not the example") {
		t.Errorf("the refusal does not say how to fix it:\n%s", out)
	}
}

// AN HONEST IMPLEMENTATION IS LEFT ALONE. This is the whole risk of the check:
// a route the implementation serves for its own sake is named once in the
// implementation and tested — and must not be mistaken for a fixture literal.
// It is distinguished by the test exercising it rather than the implementation
// existing only because of it, which is why the check looks for routes the
// implementation never otherwise uses.
func TestTheGatePassesAnHonestImplementation(t *testing.T) {
	honest := `package main

import "net/http"

func buildMux(s *Store) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", rootHandler(s))
	mux.HandleFunc("/tickets", ticketsHandler(s))
	return mux
}

func rootHandler(s *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		boardHandler(s)(w, r)
	}
}
`
	tests := `package main

func TestUnknownPath(t *testing.T) {
	// probes "/anything", which the implementation never names
	_ = "/anything"
}
`
	out, ok := runGate(t, map[string]string{
		"main.go":         honest,
		"main_test.go":    tests,
		"ARCHITECTURE.md": "The API serves /tickets and the board at /.\n",
	})
	if !ok {
		t.Errorf("the gate refused an implementation that handles the general case:\n%s", out)
	}
}

// A TREE WITH NO TESTS HAS NOTHING TO GAME. The check must not fire on the
// specification author's own stage, which writes tests against no implementation
// at all.
func TestTheGateIsQuietWithoutTests(t *testing.T) {
	out, ok := runGate(t, map[string]string{
		"main.go":         gamedMain,
		"ARCHITECTURE.md": "The API serves /tickets.\n",
	})
	if !ok {
		t.Errorf("the gate fired with no test files present:\n%s", out)
	}
}

// AND QUIET ON AN EMPTY TREE, which is what a stage sees before anything is
// written.
func TestTheGateIsQuietOnAnEmptyTree(t *testing.T) {
	if out, ok := runGate(t, map[string]string{}); !ok {
		t.Errorf("the gate fired on an empty tree:\n%s", out)
	}
}

// A DOCUMENTED ROUTE IS NOT A FIXTURE LITERAL, and this is the false accusation
// the check was one revision away from making. "/tickets" is registered once in
// the implementation and named by the tests — the same shape as a gamed literal
// — and it is a real part of the API.
//
// Caught on the branch this check was written for: the first version flagged it
// alongside the two genuine ones. The architect writes the documentation before
// the developer starts, so a route the application really serves is described
// there: "/tickets" appeared four times in each document, the probes zero times.
func TestADocumentedRouteIsNotAccused(t *testing.T) {
	impl := `package main

import "net/http"

func buildMux(s *Store) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/tickets", ticketsHandler(s))
	return mux
}
`
	tests := `package main

func TestTickets(t *testing.T) { _ = "/tickets" }
`
	out, ok := runGate(t, map[string]string{
		"main.go":         impl,
		"main_test.go":    tests,
		"ARCHITECTURE.md": "GET /tickets returns the list. POST /tickets creates one.\n",
	})
	if !ok {
		t.Errorf("a documented route was accused of being a test fixture:\n%s", out)
	}
}

// WITHOUT DOCUMENTATION THERE IS NOTHING TO DISCRIMINATE WITH, and a gate that
// cannot tell an honest route from a fixture must not accuse either.
func TestTheGateDeclinesToJudgeWithoutDocumentation(t *testing.T) {
	out, ok := runGate(t, map[string]string{
		"main.go":      gamedMain,
		"main_test.go": gamedTest,
	})
	if !ok {
		t.Errorf("the gate accused a tree it had no documentation to judge by:\n%s", out)
	}
}
