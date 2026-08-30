package agents

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runGate executes the route gate in a directory holding the given tree, the
// way the sandbox will: under sh, exit code as the verdict. TESTED BY RUNNING
// IT because the property that matters is what a shell and GNU grep do with it,
// and no amount of reading the string establishes that.
func runGate(t *testing.T, files map[string]string) (int, string) {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", routeGate)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), string(out)
	}
	t.Fatalf("the gate did not run: %v", err)
	return -1, ""
}

// THE GAP THE FIRST LOCKED-SUITE RUN SHIPPED THROUGH: HTTP source, store-only
// tests, green run, API answering 501 to everything. The gate must go red.
func TestAnHTTPTreeWithNoHandlerTestsFailsTheGate(t *testing.T) {
	code, out := runGate(t, map[string]string{
		"handlers.go":   "package main\n\nimport \"net/http\"\n\nvar _ = http.NotFound\n",
		"store.go":      "package main\n",
		"store_test.go": "package main\n\nimport \"testing\"\n\nfunc TestStore(t *testing.T) {}\n",
	})
	if code == 0 {
		t.Fatal("a store-only suite over HTTP source passed the route gate")
	}
	if !strings.Contains(out, "httptest") {
		t.Fatalf("the refusal does not say what is missing: %q", out)
	}
}

// A suite that exercises the routes passes.
func TestAnHTTPTreeWithHandlerTestsPassesTheGate(t *testing.T) {
	code, out := runGate(t, map[string]string{
		"handlers.go": "package main\n\nimport \"net/http\"\n\nvar _ = http.NotFound\n",
		"handlers_test.go": "package main\n\nimport (\n\t\"net/http/httptest\"\n\t\"testing\"\n)\n\n" +
			"func TestRoutes(t *testing.T) { _ = httptest.NewRecorder() }\n",
	})
	if code != 0 {
		t.Fatalf("a suite with handler tests failed the gate: %s", out)
	}
}

// A task with no HTTP in it must not be dead-ended by a gate about routes.
func TestANonHTTPTreeIsNotGated(t *testing.T) {
	code, out := runGate(t, map[string]string{
		"cache.go":      "package main\n\nfunc Get(k string) int { return 0 }\n",
		"cache_test.go": "package main\n\nimport \"testing\"\n\nfunc TestGet(t *testing.T) {}\n",
	})
	if code != 0 {
		t.Fatalf("a tree with no HTTP was refused by the route gate: %s", out)
	}
}

// net/http imported only by a TEST file is not "the tree serves HTTP".
func TestHTTPOnlyInTestsDoesNotTriggerTheGate(t *testing.T) {
	code, out := runGate(t, map[string]string{
		"lib.go": "package main\n\nfunc F() {}\n",
		"lib_test.go": "package main\n\nimport (\n\t\"net/http\"\n\t\"testing\"\n)\n\n" +
			"func TestF(t *testing.T) { _ = http.NotFound }\n",
	})
	if code != 0 {
		t.Fatalf("net/http in a test file alone triggered the gate: %s", out)
	}
}

// And the wiring: the test author's check carries the gate, after expected-red.
func TestTheTestAuthorsCheckCarriesTheRouteGate(t *testing.T) {
	check := maker().PlanTester(nil).Check()
	if !strings.Contains(check, "httptest") {
		t.Fatalf("the test author's check has no route gate: %q", check)
	}
	if !strings.Contains(check, "! go test") {
		t.Fatalf("the expected-red half went missing: %q", check)
	}
}
