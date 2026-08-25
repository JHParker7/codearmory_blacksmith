package main

import (
	"bytes"
	"context"
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

	for _, args := range [][]string{nil, {"--help"}, {"help"}} {
		cmd := exec.Command(bin, args...)
		// An empty environment: no config file, no tokens, nothing.
		cmd.Env = []string{"HOME=" + t.TempDir()}
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out

		if err := cmd.Run(); err != nil {
			t.Errorf("%v: exited %v\n%s", args, err, out.String())
		}
		if !strings.Contains(out.String(), "blacksmith service") {
			t.Errorf("%v: the usage does not say how to run it:\n%s", args, out.String())
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
