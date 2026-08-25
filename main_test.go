package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/department"
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// THE DEVELOPER GETS ITS OWN NUMBER because its work is not like the others': a
// review is one model call and a developer is dozens, against a sandbox it holds
// throughout. Running as many developers as reviewers fills the serving class's
// queue with the one stage that cannot be quick.
func TestTheDeveloperHasItsOwnConcurrency(t *testing.T) {
	cfg := config.Config{Concurrency: 6, DevConcurrency: 2}

	if got := concurrencyFor(cfg, workflow.RoleDev); got != 2 {
		t.Errorf("the developer runs %d at once, want its own 2", got)
	}
	for _, role := range []string{workflow.RoleReview, workflow.RoleScoping, workflow.RoleIntegrate} {
		if got := concurrencyFor(cfg, role); got != 6 {
			t.Errorf("%s runs %d at once, want the shared 6", role, got)
		}
	}

	// AN UNSET DEVELOPER NUMBER FALLS BACK rather than pinning the stage to zero,
	// which would stop it working anything at all.
	unset := config.Config{Concurrency: 6}
	if got := concurrencyFor(unset, workflow.RoleDev); got != 6 {
		t.Errorf("an unset developer concurrency gave %d", got)
	}
}

// The binary answers without a configuration, because "what does this do" must
// not require a serving stack to ask.
func TestTheBinaryExplainsItselfWithoutConfiguration(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not available")
	}
	bin := t.TempDir() + "/blacksmith"
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	for _, args := range [][]string{{"--help"}, {"help"}} {
		cmd := exec.Command(bin, args...)
		// An empty environment: no config file, no tokens, nothing.
		cmd.Env = []string{"HOME=" + t.TempDir()}
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out

		if err := cmd.Run(); err != nil {
			t.Errorf("%v: exited %v\n%s", args, err, out.String())
		}
		// BOTH COMMANDS ARE LISTED, or the window is a feature nobody finds.
		for _, want := range []string{"blacksmith service", "open the window"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%v: the usage does not mention %q:\n%s", args, want, out.String())
			}
		}
	}

	// BARE `blacksmith` OPENS THE WINDOW, so with no board configured it says
	// which variables it needs rather than printing usage. A usage screen would
	// tell a reader whose config is merely incomplete that they typed the command
	// wrong.
	bare := exec.Command(bin)
	bare.Env = []string{"HOME=" + t.TempDir()}
	var bareOut bytes.Buffer
	bare.Stdout, bare.Stderr = &bareOut, &bareOut
	if err := bare.Run(); err == nil {
		t.Errorf("the window opened with no board configured:\n%s", bareOut.String())
	}
	for _, want := range []string{"no board to read", "CODEARMORY_URL"} {
		if !strings.Contains(bareOut.String(), want) {
			t.Errorf("the window does not say what it needs (%q):\n%s", want, bareOut.String())
		}
	}

	// AN UNKNOWN COMMAND FAILS rather than silently doing nothing — a typo in a
	// unit file should not start a department that works no tickets.
	cmd := exec.Command(bin, "srevice")
	cmd.Env = []string{"HOME=" + t.TempDir()}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err == nil {
		t.Errorf("an unknown command succeeded:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "srevice") {
		t.Errorf("the failure does not name what was typed:\n%s", out.String())
	}
}

// A HOST WITH NO CONFIGURATION SAYS SO AND STOPS, rather than starting a
// department that cannot do anything.
func TestTheServiceRefusesAnUnconfiguredHost(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not available")
	}
	bin := t.TempDir() + "/blacksmith"
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	cmd := exec.Command(bin, "service")
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out

	if err := cmd.Run(); err == nil {
		t.Errorf("an unconfigured host started a department:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "configuration") {
		t.Errorf("the failure does not name the cause:\n%s", out.String())
	}
}

// SANDBOXES RUN ON A LOCAL FORGE WHERE ONE IS CONFIGURED, which is the intended
// deployment: it puts the sandbox on the same machine as the model. Falling back
// to the platform's is legitimate and must be LOUD, because a host quietly
// running its sandboxes somewhere else is slow for a reason nothing points at.
func TestSandboxesGoToTheLocalForgeWhenThereIsOne(t *testing.T) {
	local := config.Config{
		PlatformURL: "https://platform.example", PlatformToken: "tok",
		ForgeURL: "http://127.0.0.1:8080", ForgeToken: "forge-tok",
	}
	store, runner, err := clients(local)
	if err != nil {
		t.Fatalf("clients: %v", err)
	}
	if store == nil || runner == nil {
		t.Fatal("clients returned nothing")
	}

	// And with no local forge it still produces a working pair rather than
	// refusing to start.
	remote := config.Config{PlatformURL: "https://platform.example", PlatformToken: "tok"}
	if _, runner, err := clients(remote); err != nil || runner == nil {
		t.Fatalf("a host with no local forge got no runner: %v", err)
	}
}

// A PLATFORM URL THAT IS NOT A URL IS A STARTUP FAILURE, not something to
// discover on the first claim.
func TestAnInvalidPlatformURLStopsStartup(t *testing.T) {
	if _, _, err := clients(config.Config{PlatformURL: "not a url", PlatformToken: "t"}); err == nil {
		t.Error("an invalid platform URL was accepted")
	}
}

// emptyBoard is a board with nothing on it, so a dispatcher polls and finds no
// work — which is all this test needs: that every stage STARTS, and that they
// all stop when the context ends.
type emptyBoard struct{}

func (emptyBoard) List(context.Context, ticket.ListOpts) ([]ticket.Ticket, error) {
	return nil, nil
}
func (emptyBoard) Get(context.Context, string) (ticket.Ticket, error) {
	return ticket.Ticket{}, nil
}
func (emptyBoard) AddComment(context.Context, string, string) (ticket.Comment, error) {
	return ticket.Comment{}, nil
}
func (emptyBoard) MoveTo(context.Context, string, string) error { return nil }
func (emptyBoard) Claim(context.Context, string, workflow.Stage, record.Claim) error {
	return nil
}

// A dependency-free assembly: one handler that never runs, since the board is
// empty.
type idleStage struct{ role string }

func (i idleStage) Role() string           { return i.role }
func (idleStage) Class() model.Class       { return model.ClassNone }
func (idleStage) Wants(ticket.Ticket) bool { return true }
func (idleStage) Handle(context.Context, ticket.Ticket) (workflow.Outcome, string, error) {
	return workflow.OutcomeSuccess, "", nil
}

// EVERY ASSEMBLED STAGE GETS A DISPATCHER, and every dispatcher stops when the
// department does. A stage that was assembled and never started is a column
// nothing claims from — the same silent failure as one that was never assembled,
// arriving by a different route.
func TestEveryAssembledStageIsStartedAndStopsCleanly(t *testing.T) {
	assembly := department.Assembly{Stages: []department.Stage{
		{Handler: idleStage{workflow.RoleIntegrate}, Role: workflow.RoleIntegrate, Class: model.ClassNone},
		{Handler: idleStage{workflow.RoleTicketMerge}, Role: workflow.RoleTicketMerge, Class: model.ClassNone},
	}}
	cfg := config.Config{Host: "test-host", Concurrency: 1, Poll: 50 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, assembly, emptyBoard{}, nil) }()

	// Let the dispatchers come up and poll at least once.
	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the department stopped with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the department did not stop when its context ended; in-flight work would be abandoned")
	}
}

// A STAGE THAT CANNOT BE DISPATCHED STOPS STARTUP. A department that came up
// missing one stage would look healthy and quietly not work a column.
func TestAStageThatCannotBeDispatchedStopsStartup(t *testing.T) {
	// A handler whose role the routing table does not know: there is nowhere to
	// send its work.
	assembly := department.Assembly{Stages: []department.Stage{
		{Handler: idleStage{"nobody-agent"}, Role: "nobody-agent", Class: model.ClassNone},
	}}
	cfg := config.Config{Host: "test-host", Concurrency: 1, Poll: time.Second}

	err := run(context.Background(), cfg, assembly, emptyBoard{}, nil)
	if err == nil {
		t.Fatal("a stage with no route started anyway")
	}
	if !strings.Contains(err.Error(), "nobody-agent") {
		t.Errorf("the failure does not name the stage: %v", err)
	}
}

// STANDALONE MEANS THE LOCAL PLANE IS THE AUTHORITY: the store on this host
// holds the tickets, nothing is synced, and the platform is NOT CONSULTED. A
// department that reached for the platform anyway would fail on a host whose
// platform is unreachable — which is the normal state of a machine running the
// pipeline against its own plane.
func TestStandaloneReadsThisHostsOwnStore(t *testing.T) {
	cfg := config.Config{
		// A platform that must not be contacted, and would fail if it were.
		PlatformURL: "https://platform.invalid", PlatformToken: "tok",
		TicketsURL: "http://127.0.0.1:30086",
		ForgeURL:   "http://127.0.0.1:30083",
		ForgeToken: "plane-tok",
	}
	if !cfg.Standalone() {
		t.Fatal("a configured tickets URL did not put the host in standalone mode")
	}

	store, runner, err := clients(cfg)
	if err != nil {
		t.Fatalf("clients: %v", err)
	}
	if store == nil || runner == nil {
		t.Fatal("clients returned nothing")
	}
}

// AND CONNECTED IS THE OTHER SHAPE, unchanged: with no local store the platform
// holds the tickets.
func TestWithoutALocalStoreThePlatformHoldsTheTickets(t *testing.T) {
	cfg := config.Config{PlatformURL: "https://platform.example", PlatformToken: "tok"}
	if cfg.Standalone() {
		t.Fatal("a host with no tickets URL reported as standalone")
	}
	if _, _, err := clients(cfg); err != nil {
		t.Fatalf("clients: %v", err)
	}
}

// A LOCAL STORE THAT IS NOT A URL IS A STARTUP FAILURE, not something to
// discover on the first claim.
func TestAnInvalidLocalStoreURLStopsStartup(t *testing.T) {
	cfg := config.Config{PlatformURL: "https://p.example", TicketsURL: "not a url"}
	if _, _, err := clients(cfg); err == nil {
		t.Error("an invalid tickets URL was accepted")
	}
}

// THE PLANE CREDENTIAL IS A SESSION WHERE ONE CAN BE OBTAINED. The gatekeeper
// issues short tokens, so a department that cannot renew stops working when the
// first expires — and reports "unauthorized" against a healthy plane, which
// reads as a broken deployment rather than an expired session.
func TestAnEmailAndPasswordProduceARenewingPlaneCredential(t *testing.T) {
	cfg := config.Config{
		PlatformURL: "https://p.example",
		TicketsURL:  "http://127.0.0.1:30086",
		ForgeURL:    "http://127.0.0.1:30083",
		// No AGENTS_FORGE_TOKEN at all: the plane is reached by logging in.
		ForgeEmail: "agent@example.test", ForgePassword: "pw",
	}
	if _, _, err := clients(cfg); err != nil {
		t.Fatalf("clients: %v", err)
	}

	// The login address defaults to the forge's when none is given, and an
	// explicit one wins — which is what points it at the GATEKEEPER, a different
	// service on a different port from the forge.
	if got := cfg.SandboxLoginURL(); got != "http://127.0.0.1:30083" {
		t.Errorf("SandboxLoginURL = %q", got)
	}
	withLogin := cfg
	withLogin.ForgeLoginURL = "http://127.0.0.1:30081"
	if got := withLogin.SandboxLoginURL(); got != "http://127.0.0.1:30081" {
		t.Errorf("an explicit login URL was ignored: %q", got)
	}
}

// A HOST WITH NO PLANE CREDENTIAL AT ALL still starts — it may be running
// entirely against the platform — but it must not pretend it has one.
func TestAHostWithNoPlaneCredentialStillBuildsItsClients(t *testing.T) {
	cfg := config.Config{PlatformURL: "https://p.example", ForgeURL: "http://127.0.0.1:30083"}
	if _, _, err := clients(cfg); err != nil {
		t.Errorf("clients: %v", err)
	}
}

// A WINDOW FAILURE MUST OUTLIVE THE TERMINAL.
//
// The window draws on an alternate screen, and leaving it restores whatever was
// underneath — which can take the error with it. That is how the same failure
// was reported four times with nothing to go on: the cause was printed each
// time and never survived long enough to be read.
func TestAWindowFailureIsRecordedToAFile(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	noteWindowFailure(errors.New("the window needs a terminal: stdin is not one"))

	body, err := os.ReadFile(WindowLog())
	if err != nil {
		t.Fatalf("nothing was written: %v", err)
	}
	got := string(body)

	// THE CAUSE, and the three facts that separate one cause from another.
	for _, want := range []string{"stdin is not one", "term=", "stdin_tty=", "stdout_tty="} {
		if !strings.Contains(got, want) {
			t.Errorf("the note does not carry %q:\n%s", want, got)
		}
	}
}

// IT APPENDS. A second failure must not erase the first — a recurring fault is
// most readable as a series.
func TestRepeatedWindowFailuresAccumulate(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	noteWindowFailure(errors.New("first"))
	noteWindowFailure(errors.New("second"))

	body, err := os.ReadFile(WindowLog())
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Fatalf("a failure overwrote the one before it:\n%s", got)
	}
}

// RECORDING IS BEST EFFORT. It runs when something has already gone wrong, and
// failing to write a note is not worth a second error on top of the first.
func TestRecordingAFailureNeverPanics(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/proc/nonexistent/cannot-create")
	noteWindowFailure(errors.New("boom")) // must simply do nothing
}
