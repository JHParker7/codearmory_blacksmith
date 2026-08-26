package gate

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// runScript executes a generated gate script in a real shell, because the thing
// under test IS shell. A unit test that only string-matches the template proves
// the template contains a word, not that the script works.
func runScript(t *testing.T, script string, budget time.Duration) (string, int) {
	t.Helper()
	dir := t.TempDir()
	// THE SPEC GATE REFUSES A DIRECTORY WITH NO TESTS IN IT and exits before
	// anything else runs, so without this the spec cases produced no output at
	// all and read as "the timeout did not fire" when it had never been reached.
	if err := os.WriteFile(dir+"/x_test.go", []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = dir

	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.CombinedOutput()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(budget):
		_ = cmd.Process.Kill()
		t.Fatalf("the script did not return within %s; it is unbounded", budget)
	}

	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running the script: %v", err)
	}
	return string(out), code
}

// A SUITE THAT NEVER RETURNS MUST NOT CONSUME THE ATTEMPT.
//
// Measured on run 31: a specification called main() from a test, main() blocks
// in ListenAndServe, and every verification ran to Go's own ten-minute panic.
// Consecutive verifications were 10m40s apart, the attempt managed three in half
// an hour, and it died on the 45-minute deadline. The developer was not stuck; it
// was waiting.
//
// The script is generated with the real TestTimeoutSeconds but driven with a
// command that sleeps far longer, so what is proven is that the bound EXISTS and
// fires — the test itself uses a short budget by overriding the command, below.
func TestAHangingSuiteIsStoppedRatherThanWaitedOn(t *testing.T) {
	// A command that would outlast any patience, bounded by the script itself.
	script := strings.Replace(
		TestScript("sleep 600"),
		"timeout 240 ", "timeout 2 ", 1)
	if !strings.Contains(script, "timeout 2 ") {
		t.Fatal("the script has no timeout to exercise")
	}

	out, code := runScript(t, script, 30*time.Second)

	if code == 0 {
		t.Error("a suite that never returned was reported as passing")
	}
	if !strings.Contains(out, "THE TESTS DID NOT FINISH") {
		t.Errorf("the output does not say the suite was stopped:\n%s", out)
	}
	if !strings.Contains(out, "never returned") {
		t.Errorf("the notice does not distinguish hanging from failing:\n%s", out)
	}
}

// AND AN ORDINARY FAILURE IS STILL AN ORDINARY FAILURE. The bound must not
// relabel every red run as a hang — the notice is only for exit 124.
func TestAFailingSuiteIsNotReportedAsAHang(t *testing.T) {
	out, code := runScript(t, TestScript("echo 'store_test.go:12: got 2 want 1'; exit 1"),
		30*time.Second)

	if code == 0 {
		t.Error("a failing suite was reported as passing")
	}
	if strings.Contains(out, "THE TESTS DID NOT FINISH") {
		t.Errorf("an ordinary failure was relabelled as a hang:\n%s", out)
	}
	if !strings.Contains(out, "got 2 want 1") {
		t.Errorf("the real failure was lost:\n%s", out)
	}
}

// AND A PASSING SUITE STILL PASSES. The heredoc rewrite changed how the command
// is invoked, so the ordinary path needs pinning too.
func TestAPassingSuiteStillPasses(t *testing.T) {
	out, code := runScript(t, TestScript("echo ok"), 30*time.Second)

	if code != 0 {
		t.Errorf("a passing suite exited %d:\n%s", code, out)
	}
	if !strings.Contains(out, "ok") {
		t.Errorf("the command's own output was lost:\n%s", out)
	}
}

// THE COMMAND IS A CHAIN, and the heredoc must not break that. The brace group
// exists because a redirect binds to the last element of an && chain; the
// rewrite has to keep the whole chain's output together.
func TestAChainedCommandKeepsBothHalvesOfItsOutput(t *testing.T) {
	out, _ := runScript(t,
		TestScript("echo building && echo testing && exit 1"), 30*time.Second)

	for _, want := range []string{"building", "testing"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q escaped the capture:\n%s", want, out)
		}
	}
}

// THE SPECIFICATION AUTHOR'S OWN CHECK IS BOUNDED TOO. A hanging test is exactly
// what it must not hand downstream, and it is the one stage that can fix it.
func TestTheSpecGateIsAlsoBounded(t *testing.T) {
	for name, script := range map[string]string{
		"authoring": SpecScript("sleep 600", false),
		"repairing": SpecScript("sleep 600", true),
	} {
		t.Run(name, func(t *testing.T) {
			s := strings.Replace(script, "timeout 240 ", "timeout 2 ", 1)
			out, _ := runScript(t, s, 30*time.Second)
			if !strings.Contains(out, "THE TESTS DID NOT FINISH") {
				t.Errorf("a hanging suite passed the %s gate unremarked:\n%s", name, out)
			}
		})
	}
}
