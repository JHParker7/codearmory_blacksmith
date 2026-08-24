package upkeep

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

type board struct {
	tickets []ticket.Ticket
	created []ticket.Ticket
	seq     int

	failList   bool
	listDelay  time.Duration
	failCreate bool
}

func (b *board) List(ctx context.Context, _ ticket.ListOpts) ([]ticket.Ticket, error) {
	if b.listDelay > 0 {
		select {
		case <-time.After(b.listDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if b.failList {
		return nil, errors.New("the store could not be reached")
	}
	return b.tickets, nil
}

func (b *board) Create(_ context.Context, t ticket.Ticket) (ticket.Ticket, error) {
	if b.failCreate {
		return ticket.Ticket{}, errors.New("the store could not be reached")
	}
	b.seq++
	t.ID = fmt.Sprintf("t-%d", b.seq)
	b.created = append(b.created, t)
	b.tickets = append(b.tickets, t)
	return t, nil
}

func repo() config.Repo {
	return config.Repo{
		LintCommand: "go vet ./...",
		TestCommand: "go test ./...",
	}
}

// LINT FIRST. A run only going to fail on style stops before paying for the test
// suite, and the linter's complaint lands at the top of what the agent reads
// rather than under a screen of test results.
func TestTheUpkeepGateRunsTheLinterBeforeTheTests(t *testing.T) {
	got := WithMaintenance(repo())
	if got.TestCommand != "go vet ./... && go test ./..." {
		t.Errorf("TestCommand = %q", got.TestCommand)
	}
	if !strings.HasPrefix(got.TestCommand, "go vet") {
		t.Error("the tests run before the linter; a style failure would pay for the whole suite first")
	}
}

func TestTheUpkeepGateCopesWithEitherCommandMissing(t *testing.T) {
	// No linter: there is no upkeep gate to build, and the ordinary one stands.
	noLint := config.Repo{TestCommand: "go test ./..."}
	if got := WithMaintenance(noLint); got.TestCommand != "go test ./..." {
		t.Errorf("TestCommand = %q, want it untouched", got.TestCommand)
	}
	// No tests: the linter IS the gate, rather than an empty command that passes
	// whatever the branch does.
	noTests := config.Repo{LintCommand: "go vet ./..."}
	if got := WithMaintenance(noTests); got.TestCommand != "go vet ./..." {
		t.Errorf("TestCommand = %q, want the linter", got.TestCommand)
	}
}

// ONLY THE LINTER'S FINDINGS. The same analysis carries a dependency scan, whose
// findings are in third-party code and are not this repository's to fix, and a
// static analysis pass whose output is a lead rather than a defect.
func TestOnlyTheLintersSectionBecomesUpkeep(t *testing.T) {
	analysis := strings.Join([]string{
		"## " + LintSectionLabel,
		"main.go:12:2: unreachable code",
		"store.go:40:1: exported function needs a comment",
		"",
		"## SCA (dependencies, advisory)",
		"github.com/x/y@v1.0.0: CVE-2024-1111",
		"",
		"## SAST (static analysis, advisory)",
		"main.go:8: potential hardcoded credential",
	}, "\n")

	got := LintFindings(analysis)
	if !strings.Contains(got, "unreachable code") {
		t.Errorf("the linter's findings were lost: %q", got)
	}
	if strings.Contains(got, "CVE-2024-1111") {
		t.Error("a dependency finding became upkeep; it is not this repository's to fix")
	}
	if strings.Contains(got, "hardcoded credential") {
		t.Error("a static analysis lead became upkeep; it is a lead rather than a defect")
	}
}

func TestAnAnalysisWithNoLintSectionYieldsNothing(t *testing.T) {
	for _, in := range []string{"", "## SCA (dependencies, advisory)\nnothing here", "plain prose"} {
		if got := LintFindings(in); got != "" {
			t.Errorf("LintFindings(%q) = %q, want empty", in, got)
		}
	}
}

// The last section runs to the end rather than being cut short.
func TestTheLintSectionRunsToTheEndWhenItIsLast(t *testing.T) {
	analysis := "## SCA (dependencies, advisory)\nsomething\n\n## " + LintSectionLabel + "\na finding\nanother"
	got := LintFindings(analysis)
	if !strings.Contains(got, "a finding") || !strings.Contains(got, "another") {
		t.Errorf("LintFindings() = %q, want both lines", got)
	}
}

// FINDINGS COME FROM A REAL RUN, so they exist only when they are real: nothing
// to lint means no ticket rather than an agent inventing work.
func TestNothingIsFiledWithoutRealFindings(t *testing.T) {
	cases := map[string]struct {
		repo     config.Repo
		findings string
	}{
		"no findings":      {repo(), ""},
		"blank findings":   {repo(), "   \n  "},
		"no linter at all": {config.Repo{TestCommand: "go test ./..."}, "main.go:1: something"},
	}
	for name, c := range cases {
		b := &board{}
		filed, err := Record(context.Background(), b, "b-1", c.repo, c.findings, "agent/t-1")
		if err != nil {
			t.Errorf("%s: Record: %v", name, err)
		}
		if filed {
			t.Errorf("%s: a ticket was filed", name)
		}
		if len(b.created) != 0 {
			t.Errorf("%s: %d tickets were opened", name, len(b.created))
		}
	}
}

func TestFindingsBecomeALowPriorityTicket(t *testing.T) {
	b := &board{}
	filed, err := Record(context.Background(), b, "b-1", repo(), "main.go:12:2: unreachable code", "agent/t-1")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !filed {
		t.Fatal("real findings were not filed")
	}
	if len(b.created) != 1 {
		t.Fatalf("opened %d tickets", len(b.created))
	}

	got := b.created[0]
	if got.Title != Title {
		t.Errorf("title = %q", got.Title)
	}
	if got.Status != workflow.ColReadyForMaintenance {
		t.Errorf("status = %q, want the upkeep queue", got.Status)
	}
	// LOWEST THERE IS. Nothing asked for this.
	if got.Priority != "low" {
		t.Errorf("priority = %q, want low", got.Priority)
	}
	if got.Board() != "b-1" {
		t.Errorf("board = %q", got.Board())
	}
	// THE FINDINGS THEMSELVES, not just the command that produced them:
	// re-deriving them would cost a sandbox to learn what is already known.
	if !strings.Contains(got.Description, "unreachable code") {
		t.Error("the brief does not carry the findings")
	}
	if !strings.Contains(got.Description, "agent/t-1") {
		t.Error("the brief does not say which branch they came from")
	}
}

// ONE OPEN TICKET AT A TIME. The findings are cumulative — the next review
// reports whatever is still there — so a second ticket is the same work twice,
// and a board collecting one per merge is a board nobody reads.
func TestASecondTicketIsNotOpenedWhileOneIsStillOpen(t *testing.T) {
	b := &board{}
	if _, err := Record(context.Background(), b, "b-1", repo(), "a finding", "agent/t-1"); err != nil {
		t.Fatalf("Record: %v", err)
	}

	filed, err := Record(context.Background(), b, "b-1", repo(), "another finding", "agent/t-2")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if filed {
		t.Error("a second upkeep ticket was opened while one was still open")
	}
	if len(b.created) != 1 {
		t.Errorf("opened %d tickets", len(b.created))
	}
}

// ...but a FINISHED one does not block the next. The findings are cumulative and
// the work is never done for good.
func TestAFinishedTicketDoesNotBlockTheNext(t *testing.T) {
	for _, done := range []string{workflow.ColDone, workflow.ColBlocked} {
		b := &board{tickets: []ticket.Ticket{{ID: "t-old", Title: Title, Status: done}}}
		filed, err := Record(context.Background(), b, "b-1", repo(), "a finding", "agent/t-1")
		if err != nil {
			t.Fatalf("Record: %v", err)
		}
		if !filed {
			t.Errorf("a ticket in %q blocked the next one", done)
		}
	}
}

func TestAFailureToReadOrOpenIsReported(t *testing.T) {
	if _, err := Record(context.Background(), &board{failList: true}, "b-1", repo(), "x", "b"); err == nil {
		t.Error("a failed listing was reported as nothing to do")
	}
	if _, err := Record(context.Background(), &board{failCreate: true}, "b-1", repo(), "x", "b"); err == nil {
		t.Error("a failed create was reported as success")
	}
}

// GATED AT THE CLAIM, NOT AT THE OPENING. A board empty at the moment a ticket is
// opened says nothing about the next ten minutes.
func TestUpkeepStandsDownWhileAnythingElseIsLive(t *testing.T) {
	mine := ticket.Ticket{ID: "t-up", Title: Title, Status: workflow.ColReadyForMaintenance}

	t.Run("a request is waiting", func(t *testing.T) {
		b := &board{tickets: []ticket.Ticket{
			mine,
			{ID: "t-1", Title: "Add rate limiting", Status: workflow.ColReadyForDev},
		}}
		if NewGate(b, "b-1").Wants(mine) {
			t.Error("upkeep claimed work while a request was waiting")
		}
	})

	t.Run("a request is in flight", func(t *testing.T) {
		b := &board{tickets: []ticket.Ticket{
			mine,
			{ID: "t-1", Title: "Add rate limiting", Status: workflow.ColInDev},
		}}
		if NewGate(b, "b-1").Wants(mine) {
			t.Error("upkeep claimed work while a request was being worked")
		}
	})

	t.Run("everything else is finished", func(t *testing.T) {
		b := &board{tickets: []ticket.Ticket{
			mine,
			{ID: "t-1", Title: "Add rate limiting", Status: workflow.ColDone},
			{ID: "t-2", Title: "Something stuck", Status: workflow.ColBlocked},
		}}
		if !NewGate(b, "b-1").Wants(mine) {
			t.Error("upkeep stood down with nothing live on the board")
		}
	})
}

// ANOTHER UPKEEP TICKET IS NOT SOMEONE WAITING, or upkeep would block itself.
func TestUpkeepDoesNotBlockItself(t *testing.T) {
	mine := ticket.Ticket{ID: "t-up", Title: Title, Status: workflow.ColReadyForMaintenance}
	b := &board{tickets: []ticket.Ticket{
		mine,
		{ID: "t-up2", Title: Title, Status: workflow.ColReadyForMaintenance},
	}}
	if !NewGate(b, "b-1").Wants(mine) {
		t.Error("upkeep stood down because another upkeep ticket existed")
	}
}

// A READ FAILURE MEANS NO. The board is the only thing that knows whether anyone
// is waiting, and upkeep proceeding on a guess is the failure this prevents.
func TestUpkeepStandsDownWhenItCannotSeeTheBoard(t *testing.T) {
	mine := ticket.Ticket{ID: "t-up", Title: Title, Status: workflow.ColReadyForMaintenance}
	if NewGate(&board{failList: true}, "b-1").Wants(mine) {
		t.Error("upkeep claimed work on a guess")
	}
}

// THE GATE RUNS ON EVERY POLL of an otherwise idle dispatcher, so it must not be
// able to hang one.
func TestTheClaimGateCannotHangADispatcher(t *testing.T) {
	mine := ticket.Ticket{ID: "t-up", Title: Title, Status: workflow.ColReadyForMaintenance}
	g := NewGate(&board{listDelay: time.Hour}, "b-1")
	g.Timeout = 20 * time.Millisecond

	done := make(chan bool, 1)
	go func() { done <- g.Wants(mine) }()

	select {
	case got := <-done:
		if got {
			t.Error("a board read that timed out was treated as an empty board")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the claim gate hung on a store that never answered")
	}
}

// THE GATE IS THE AUTHORITY, NOT THE LIST. An agent told otherwise edits until
// the list is satisfied rather than until the gate is.
func TestTheBriefSaysTheGateDecidesAndToChangeLittle(t *testing.T) {
	got := Brief(repo(), "main.go:1: something", "agent/t-1")

	for _, want := range []string{
		"lowest priority",
		"the gate is the authority",
		"Change as little as possible",
		"go vet ./... && go test ./...",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the brief does not say %q:\n%s", want, got)
		}
	}
	// The tests are the specification and run in the same gate.
	if !strings.Contains(got, "not a fix") {
		t.Error("the brief does not forbid changing behaviour to satisfy the linter")
	}
}

// A very large finding list must not become the whole prompt.
func TestTheBriefBoundsTheFindingsItQuotes(t *testing.T) {
	huge := strings.Repeat("main.go:1:1: a finding\n", 5000)
	got := Brief(repo(), huge, "agent/t-1")
	if len([]rune(got)) > 6000 {
		t.Errorf("the brief is %d runes; a finding list became the whole prompt", len([]rune(got)))
	}
	if !strings.Contains(got, "…") {
		t.Error("the quoted findings were clipped without saying so")
	}
}
