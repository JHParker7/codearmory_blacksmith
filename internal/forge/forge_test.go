package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/transport"
)

// fakeForge is forge's execution and lease API, in memory, over real HTTP.
type fakeForge struct {
	mu sync.Mutex

	// steps is the status an execution reports on each successive poll, so a test
	// can say "running, running, completed" and have the poll loop actually walk
	// it.
	steps    []string
	polls    int
	exitCode int
	stdout   string

	leaseSteps  []string
	leasePolls  int
	leaseDetail string

	cancelled []string
	submitted []Spec
	created   []LeaseSpec
	paths     []string

	// failNextRun and failEveryRun make POST /executions answer 409 with that
	// body, which is how forge reports a lease it no longer has. failCreate makes
	// booting a replacement fail.
	failNextRun  string
	failEveryRun string
	failCreate   error

	// leaseSeq gives each lease a DISTINCT id, so a test can tell a replacement
	// from the container it replaced. The first is still l-1.
	leaseSeq int
}

func newFakeForge(t *testing.T) (*fakeForge, *Client) {
	t.Helper()
	f := &fakeForge{steps: []string{StatusCompleted}, leaseSteps: []string{LeaseReady}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	c := Local(srv.URL, transport.Static("tok"))
	c.PollMin, c.PollMax = time.Millisecond, 2*time.Millisecond
	c.http.Backoff = time.Millisecond
	return f, c
}

func (f *fakeForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	f.mu.Unlock()

	switch {
	case r.URL.Path == "/executions" && r.Method == http.MethodPost:
		var spec Spec
		json.NewDecoder(r.Body).Decode(&spec)
		f.mu.Lock()
		f.submitted = append(f.submitted, spec)
		fail := f.failEveryRun
		if fail == "" {
			fail, f.failNextRun = f.failNextRun, ""
		}
		f.mu.Unlock()
		if fail != "" {
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(fail))
			return
		}
		json.NewEncoder(w).Encode(Execution{ExecutionID: "x-1", Status: StatusPending})

	case strings.HasPrefix(r.URL.Path, "/executions/") && r.Method == http.MethodGet:
		f.mu.Lock()
		i := f.polls
		if i >= len(f.steps) {
			i = len(f.steps) - 1
		}
		status := f.steps[i]
		f.polls++
		code, out := f.exitCode, f.stdout
		f.mu.Unlock()

		exec := Execution{ExecutionID: "x-1", Status: status}
		if Terminal(status) {
			exec.ExitCode, exec.Stdout = &code, &out
		}
		json.NewEncoder(w).Encode(exec)

	case strings.HasPrefix(r.URL.Path, "/executions/") && r.Method == http.MethodDelete:
		f.mu.Lock()
		f.cancelled = append(f.cancelled, strings.TrimPrefix(r.URL.Path, "/executions/"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	case r.URL.Path == "/leases" && r.Method == http.MethodPost:
		var spec LeaseSpec
		json.NewDecoder(r.Body).Decode(&spec)
		f.mu.Lock()
		f.created = append(f.created, spec)
		boom := f.failCreate
		f.leaseSeq++
		id := fmt.Sprintf("l-%d", f.leaseSeq)
		f.mu.Unlock()
		if boom != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(boom.Error()))
			return
		}
		json.NewEncoder(w).Encode(Lease{LeaseID: id, Status: LeaseStarting})

	case strings.HasPrefix(r.URL.Path, "/leases/") && r.Method == http.MethodGet:
		f.mu.Lock()
		i := f.leasePolls
		if i >= len(f.leaseSteps) {
			i = len(f.leaseSteps) - 1
		}
		status, detail := f.leaseSteps[i], f.leaseDetail
		f.leasePolls++
		f.mu.Unlock()
		id := strings.TrimPrefix(r.URL.Path, "/leases/")
		json.NewEncoder(w).Encode(Lease{LeaseID: id, Status: status, Detail: detail})

	case strings.HasPrefix(r.URL.Path, "/leases/") && r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)

	default:
		http.NotFound(w, r)
	}
}

func (f *fakeForge) seenCancels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.cancelled))
	copy(out, f.cancelled)
	return out
}

func TestRunWaitsForTheExecutionToFinish(t *testing.T) {
	f, c := newFakeForge(t)
	f.steps = []string{StatusPending, StatusRunning, StatusRunning, StatusCompleted}
	f.stdout = "ok"

	res, err := c.Run(context.Background(), nil, Spec{Image: "img", Command: []string{"sh", "-c", "true"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.OK() {
		t.Errorf("result = %+v, want a clean run", res)
	}
	if res.Stdout != "ok" {
		t.Errorf("stdout = %q", res.Stdout)
	}
	if f.polls < len(f.steps) {
		t.Errorf("the loop polled %d times for %d states; it did not wait", f.polls, len(f.steps))
	}
}

// "THE SANDBOX RAN AND THE TESTS FAILED" IS NOT AN ERROR. Collapsing the two
// would make a red test suite indistinguishable from an unreachable forge, and
// an agent must reason about the first and give up on the second.
func TestANonZeroExitIsAResultNotAnError(t *testing.T) {
	f, c := newFakeForge(t)
	f.exitCode = 1
	f.stdout = "FAIL\tstore\t0.2s"

	res, err := c.Run(context.Background(), nil, Spec{Image: "img", Command: []string{"go", "test", "./..."}})
	if err != nil {
		t.Fatalf("a failing test suite came back as an error: %v", err)
	}
	if res.OK() {
		t.Error("a non-zero exit reported OK")
	}
	if res.ExitCode != 1 || !strings.Contains(res.Stdout, "FAIL") {
		t.Errorf("result = %+v", res)
	}
}

// A status is not enough on its own: forge reports COMPLETED for a command that
// ran and failed, so OK is the status AND the exit code.
func TestOKNeedsBothTheStatusAndTheExitCode(t *testing.T) {
	cases := []struct {
		res  Result
		want bool
	}{
		{Result{Status: StatusCompleted, ExitCode: 0}, true},
		{Result{Status: StatusCompleted, ExitCode: 1}, false},
		{Result{Status: StatusFailed, ExitCode: 0}, false},
		{Result{Status: StatusTimedOut, ExitCode: 0}, false},
		// The word matters: forge says "completed", never "succeeded", and polling
		// for the wrong one spins until the lease is reaped.
		{Result{Status: "succeeded", ExitCode: 0}, false},
	}
	for _, c := range cases {
		if got := c.res.OK(); got != c.want {
			t.Errorf("%+v.OK() = %v, want %v", c.res, got, c.want)
		}
	}
}

// AN ABANDONED SANDBOX HOLDS A RUNNER SLOT until it times out on its own, so the
// caller going away has to take the work with it. A workstation is shut down
// often enough for this to matter.
func TestCancellationCancelsTheExecution(t *testing.T) {
	f, c := newFakeForge(t)
	f.steps = []string{StatusRunning}
	c.PollMin, c.PollMax = 5*time.Millisecond, 5*time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(15 * time.Millisecond)
		cancel()
	}()

	_, err := c.Run(ctx, nil, Spec{Image: "img", Command: []string{"sleep", "60"}})
	if err == nil {
		t.Fatal("a cancelled run returned no error")
	}
	if got := f.seenCancels(); len(got) == 0 {
		t.Fatal("the execution was left running after the caller went away")
	}
	// The message must say the sandbox was cancelled, not merely that a context
	// ended: the two send a reader to different places.
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("err = %v", err)
	}
}

// The cancellation runs on a DETACHED context. Reusing the cancelled one would
// cancel the cancellation and leave the sandbox running, which is the whole
// failure this exists to avoid.
func TestTheCancelRequestSurvivesTheCancelledContext(t *testing.T) {
	f, c := newFakeForge(t)
	f.steps = []string{StatusRunning}
	c.PollMin, c.PollMax = 5*time.Millisecond, 5*time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(15 * time.Millisecond)
		cancel()
	}()
	c.Run(ctx, nil, Spec{Image: "img", Command: []string{"sleep", "60"}})

	// Give the detached call its own moment; it must not have been cancelled with
	// the parent.
	time.Sleep(20 * time.Millisecond)
	if got := f.seenCancels(); len(got) != 1 || got[0] != "x-1" {
		t.Errorf("cancellations = %q, want the one execution", got)
	}
}

// A TRANSIENT READ MUST NOT ABANDON A RUNNING SANDBOX. The transport has already
// spent its own retries; giving up here would cancel work that is still fine.
func TestATransientPollFailureKeepsWaiting(t *testing.T) {
	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/executions" && r.Method == http.MethodPost:
			json.NewEncoder(w).Encode(Execution{ExecutionID: "x-1", Status: StatusPending})
		case r.Method == http.MethodDelete:
			t.Error("a running sandbox was cancelled after a transient read failure")
		default:
			polls++
			if polls < 3 {
				http.Error(w, "gateway", http.StatusBadGateway)
				return
			}
			code := 0
			json.NewEncoder(w).Encode(Execution{ExecutionID: "x-1", Status: StatusCompleted, ExitCode: &code})
		}
	}))
	t.Cleanup(srv.Close)

	c := Local(srv.URL, transport.Static("tok"))
	c.PollMin, c.PollMax = time.Millisecond, time.Millisecond
	c.http.MaxRetries = 0

	res, err := c.Run(context.Background(), nil, Spec{Image: "img", Command: []string{"true"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.OK() {
		t.Errorf("result = %+v", res)
	}
}

// A failure that will NOT improve must stop the loop rather than polling
// forever: a deleted execution is not coming back.
func TestAPermanentPollFailureGivesUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/executions" && r.Method == http.MethodPost {
			json.NewEncoder(w).Encode(Execution{ExecutionID: "x-1", Status: StatusPending})
			return
		}
		http.Error(w, "gone", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	c := Local(srv.URL, transport.Static("tok"))
	c.PollMin, c.PollMax = time.Millisecond, time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := c.Run(context.Background(), nil, Spec{Image: "img", Command: []string{"true"}})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a vanished execution reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the poll loop never gave up on an execution that is gone")
	}
}

func TestSubmitRefusesACommandItCannotRun(t *testing.T) {
	_, c := newFakeForge(t)
	ctx := context.Background()

	if _, err := c.Submit(ctx, Spec{Command: []string{"true"}}); err == nil {
		t.Error("a one-shot execution with no image was submitted")
	}
	if _, err := c.Submit(ctx, Spec{Image: "img"}); err == nil {
		t.Error("an execution with no command was submitted")
	}
	// A LEASED COMMAND INHERITS ITS IMAGE from the sandbox it runs in, so the
	// image requirement applies only to the one-shot form.
	if _, err := c.Submit(ctx, Spec{LeaseID: "l-1", Command: []string{"true"}}); err != nil {
		t.Errorf("a leased execution was rejected for having no image: %v", err)
	}
}

func TestPollIntervalBacksOffAndStops(t *testing.T) {
	lo, hi := 500*time.Millisecond, 2*time.Second
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second}
	for i, w := range want {
		if got := pollInterval(i, lo, hi); got != w {
			t.Errorf("pollInterval(%d) = %v, want %v", i, got, w)
		}
	}
	// A large attempt count must not overflow into a tiny or negative interval,
	// which would turn the backoff into a hot loop.
	if got := pollInterval(64, lo, hi); got != hi {
		t.Errorf("pollInterval(64) = %v, want the cap %v", got, hi)
	}
}

func TestSummariseBoundsTheCommand(t *testing.T) {
	long := Spec{RunnerClass: "dev", Command: []string{strings.Repeat("x", 500)}}
	got := Summarise(long)
	if len([]rune(got)) > 260 {
		t.Errorf("a 500-character command rendered as %d runes", len([]rune(got)))
	}
	if !strings.HasPrefix(got, "dev: ") {
		t.Errorf("the runner class is not named: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("a clipped command does not show it was clipped")
	}
}
