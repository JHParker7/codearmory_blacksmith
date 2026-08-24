package forge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/transport"
)

// AN APOSTROPHE IN PROSE CLOSED A QUOTE AND COST A REAL OUTAGE. An evidence
// label reading "this project's own code" left the script with an unbalanced
// quote, the shell exited 2 on a syntax error, nothing ran, and every security
// review failed with an empty diff and no output to explain it.
func TestQuoteSurvivesAnApostrophe(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}

	for _, s := range []string{
		"this project's own code",
		"plain",
		"",
		`a "double" quote`,
		`back\slash`,
		"it's a 'nested' one",
		"a — dash and an é",
		"$(rm -rf /)",
		"`backticks`",
		"new\nline",
	} {
		// The quoted word must reach the shell as EXACTLY the original string.
		out, err := exec.Command("sh", "-c", "printf %s "+Quote(s)).Output()
		if err != nil {
			t.Errorf("Quote(%q) produced a script the shell rejected: %v", s, err)
			continue
		}
		if string(out) != s {
			t.Errorf("Quote(%q) round-tripped as %q", s, string(out))
		}
	}
}

// A value that closes the quote must not be able to run anything. This is the
// same property as above stated as the risk it removes.
func TestQuoteCannotBeEscapedInto(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	// A side effect, not a substring: the literal value contains its own marker
	// word, so grepping the output proves nothing. If the quoting fails, this
	// command runs and the file appears.
	marker := filepath.Join(t.TempDir(), "escaped")
	hostile := `'; touch ` + marker + `; echo '`

	out, err := exec.Command("sh", "-c", "printf %s "+Quote(hostile)).Output()
	if err != nil {
		t.Fatalf("the shell rejected the script: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a value escaped its quoting and ran a command")
	}
	if string(out) != hostile {
		t.Errorf("round-tripped as %q, want the literal value", out)
	}
}

// THE PREAMBLE IS WHAT MAKES A READ-ONLY ROOT USABLE. Every variable in it was
// added because a toolchain failed without it, and the GOPATH one was missed the
// first time — `go run tool@latest` failed on the module checksum cache with a
// message that reads like a corrupt toolchain rather than a read-only mount.
func TestThePreambleRedirectsEveryCacheOffTheReadOnlyRoot(t *testing.T) {
	for _, want := range []string{
		"HOME=/tmp",
		"GOPATH=/tmp/go",
		"XDG_CACHE_HOME=/tmp/.cache",
		"GOCACHE=/tmp/.cache/go-build",
		"GOMODCACHE=/tmp/.cache/go-mod",
		"npm_config_cache=/tmp/.cache/npm",
		"COMMIT_MSG_FILE=",
	} {
		if !strings.Contains(Preamble, want) {
			t.Errorf("the preamble does not set %s; a tool that writes there fails on the read-only root", want)
		}
	}
	// set -e, or a failing step is followed by every later one against a tree it
	// did not prepare.
	if !strings.HasPrefix(Preamble, "set -e") {
		t.Error("the preamble does not stop on the first failure")
	}
	if !strings.Contains(Preamble, "cd /tmp/work") {
		t.Error("the preamble does not enter the writable working directory")
	}
}

func TestThePreambleIsValidShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	if out, err := exec.Command("sh", "-n", "-c", Preamble).CombinedOutput(); err != nil {
		t.Fatalf("the preamble is not valid shell: %v\n%s", err, out)
	}
}

// A CLASS IS DECLARED, NOT ASSUMED. Forge's seeded classes are sized for
// containers and the sandbox plane runs microVMs; left to the default, a
// developer task compiles inside a 256 MB guest and is OOM-killed partway
// through — which surfaces as a TEST FAILURE the agent then tries to fix in the
// code, burning its whole budget on a problem outside the repository.
func TestEnsureRunnerClassUpdatesOneThatExists(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(RunnerClass{Name: "dev"})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := Local(srv.URL, transport.Static("tok"))
	if err := c.EnsureRunnerClass(context.Background(), RunnerClass{Name: "dev", MemoryMB: 8192}); err != nil {
		t.Fatalf("EnsureRunnerClass: %v", err)
	}
	if len(methods) != 2 || methods[0] != http.MethodGet || methods[1] != http.MethodPut {
		t.Errorf("methods = %v, want a read then an update", methods)
	}
}

func TestEnsureRunnerClassCreatesOneThatDoesNot(t *testing.T) {
	var methods []string
	var created RunnerClass
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.Method == http.MethodGet {
			http.Error(w, "no such class", http.StatusNotFound)
			return
		}
		json.NewDecoder(r.Body).Decode(&created)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)

	c := Local(srv.URL, transport.Static("tok"))
	want := RunnerClass{Name: "dev", MemoryMB: 8192, CPUMillicores: 4000, Backend: "microvm", Enabled: true}
	if err := c.EnsureRunnerClass(context.Background(), want); err != nil {
		t.Fatalf("EnsureRunnerClass: %v", err)
	}
	if len(methods) != 2 || methods[1] != http.MethodPost {
		t.Errorf("methods = %v, want a read then a create", methods)
	}
	// The SIZING must survive the round trip, or the class is created unsized and
	// the OOM is back.
	if created.MemoryMB != want.MemoryMB || created.CPUMillicores != want.CPUMillicores {
		t.Errorf("created %+v, want the configured sizing", created)
	}
}

func TestEnsureRunnerClassRefusesAnUnnamedClass(t *testing.T) {
	_, c := newFakeForge(t)
	if err := c.EnsureRunnerClass(context.Background(), RunnerClass{MemoryMB: 8192}); err == nil {
		t.Error("a runner class with no name was accepted")
	}
}

// A failure reading the class must not be read as "it does not exist" and turned
// into a create: that would POST over a class somebody is using.
func TestEnsureRunnerClassStopsOnAnUnexpectedFailure(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		http.Error(w, "no grant", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	c := Local(srv.URL, transport.Static("tok"))
	if err := c.EnsureRunnerClass(context.Background(), RunnerClass{Name: "dev"}); err == nil {
		t.Fatal("a denied read was treated as a missing class")
	}
	for _, m := range methods {
		if m == http.MethodPost || m == http.MethodPut {
			t.Errorf("a write was attempted after a denied read: %v", methods)
		}
	}
}

// FORGE ENFORCES AN ALLOWLIST, and a rejected image comes back as a bare "image
// not allowed" naming neither the image nor the alternatives. An agent picking
// one needs to see the list, and a person debugging one needs it more.
func TestImagesReturnsTheAllowlist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images" {
			t.Errorf("asked for %q", r.URL.Path)
		}
		json.NewEncoder(w).Encode([]string{"golang:1.25", "node:22"})
	}))
	t.Cleanup(srv.Close)

	c := Local(srv.URL, transport.Static("tok"))
	got, err := c.Images(context.Background())
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	if len(got) != 2 || got[0] != "golang:1.25" {
		t.Errorf("images = %v", got)
	}
}

// THE SERVICE PREFIX IS FIXED BY THE CONSTRUCTOR. Same trap as the ticket store:
// an unprefixed path against the platform does not 404, it returns the portal's
// HTML with a 200.
func TestARoutedClientCarriesTheServicePrefix(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		json.NewEncoder(w).Encode([]string{})
	}))
	t.Cleanup(srv.Close)

	if _, err := Routed(srv.URL, transport.Static("tok")).Images(context.Background()); err != nil {
		t.Fatalf("Images: %v", err)
	}
	if _, err := Local(srv.URL, transport.Static("tok")).Images(context.Background()); err != nil {
		t.Fatalf("Images: %v", err)
	}
	if paths[0] != servicePrefix+"/images" {
		t.Errorf("a routed client requested %q, want %q", paths[0], servicePrefix+"/images")
	}
	if paths[1] != "/images" {
		t.Errorf("a local client requested %q; nothing routes by service name there", paths[1])
	}
}

// ReleaseLease is what stops a finished sandbox holding a runner class's worth of
// memory until its timeout collects it.
func TestReleaseLeaseDestroysTheSandbox(t *testing.T) {
	var deleted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = strings.TrimPrefix(r.URL.Path, "/leases/")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	c := Local(srv.URL, transport.Static("tok"))
	if err := c.ReleaseLease(context.Background(), "l-1"); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if deleted != "l-1" {
		t.Errorf("released %q", deleted)
	}
}

// An identifier with a slash or a space in it must not be able to reach a
// different endpoint than the one named.
func TestLeaseAndExecutionIDsAreEscapedIntoThePath(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.EscapedPath()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	c := Local(srv.URL, transport.Static("tok"))
	if err := c.ReleaseLease(context.Background(), "l-1/../executions/x-9"); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if strings.Contains(path, "/executions/") {
		t.Errorf("an identifier walked out of its path segment: %q", path)
	}
}

func TestPollBoundsFallBackToTheDefaults(t *testing.T) {
	c := &Client{}
	lo, hi := c.pollBounds()
	if lo != DefaultPollMin || hi != DefaultPollMax {
		t.Errorf("bounds = %v/%v, want the defaults", lo, hi)
	}

	c.PollMin, c.PollMax = -1, -1
	if lo, hi = c.pollBounds(); lo != DefaultPollMin || hi != DefaultPollMax {
		t.Errorf("negative bounds gave %v/%v; a non-positive interval is a hot loop", lo, hi)
	}
}
