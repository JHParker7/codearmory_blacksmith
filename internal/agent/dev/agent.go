package dev

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/forge"
	"github.com/code-armory-app/blacksmith/internal/gate"
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/transcript"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// Sandboxes hands out a held container for one ticket.
type Sandboxes interface {
	Acquire(ctx context.Context, spec forge.SandboxSpec) (Box, error)
}

// Box is the sandbox this loop runs in.
//
// AN INTERFACE, so the loop can be driven without a network. What is being
// tested here is a sequence of decisions — read, write, verify, stop — and a
// test that has to stand up forge to check the order of two of them is a test
// nobody writes. *forge.Sandbox satisfies it.
type Box interface {
	Run(ctx context.Context, rec forge.Recorder, script string) (forge.Result, error)
	RunOnBranch(ctx context.Context, rec forge.Recorder, branch, script string) (forge.Result, error)
	AdoptBranch(branch string)
	AlsoMerge(script string)
	Release(ctx context.Context)
}

// Store is the board operations this stage needs.
type Store interface {
	AddComment(ctx context.Context, id, body string) (ticket.Comment, error)
}

// Agent runs the developer's loop for one ticket.
type Agent struct {
	gw       Gateway
	boxes    Sandboxes
	store    Store
	class    model.Class
	repo     config.Repo
	mode     Mode
	role     string
	prompt   string
	maxTurns int

	// tools says whether this class's backend parses tool calls. See
	// BuildRequest: the grammar is the fallback, not the preference.
	tools bool
}

// Options configure one stage built on this loop.
type Options struct {
	Mode         Mode
	Role         string
	SystemPrompt string
	MaxTurns     int
	Tools        bool
}

// New builds a stage.
func New(gw Gateway, boxes Sandboxes, store Store, class model.Class, repo config.Repo, o Options) *Agent {
	turns := o.MaxTurns
	if turns <= 0 {
		turns = DefaultMaxIterations
	}
	return &Agent{
		gw: gw, boxes: boxes, store: store, class: class, repo: repo,
		mode: o.Mode, role: o.Role, prompt: o.SystemPrompt, maxTurns: turns, tools: o.Tools,
	}
}

func (a *Agent) Role() string       { return a.role }
func (a *Agent) Class() model.Class { return a.class }

// Wants accepts everything in its queue: the column is the predicate.
func (a *Agent) Wants(ticket.Ticket) bool { return true }

// BranchFor names the branch this stage works on.
//
// A SECTION WORKS ITS TASK'S BRANCH. That is the whole assembly mechanism: every
// section of a task writes one tree, so the task's branch ends up holding the
// complete specification rather than one slice of it per branch.
//
// ONLY A SECTION. The parent redirect applied to development too once, and
// because a TASK also has a parent — the request the breakdown came from — every
// developer was sent to the REQUEST's branch, where nothing was written.
//
// Measured on r73, and it is silent: eight sections wrote their tests to four
// task branches, all four developers were pointed at agent/<request> where there
// were none, each finished in seven records against an empty tree, the review
// reported "empty diff" and the integrator merged it. The whole board reported
// success in 10.3 minutes having delivered one struct.
func (a *Agent) BranchFor(t ticket.Ticket) string {
	prefix := a.repo.BranchPrefix
	if prefix == "" {
		prefix = "agent/"
	}
	id := t.ID
	if a.mode == ModeTest && t.Parent() != "" {
		id = t.Parent()
	}
	return prefix + id
}

// Handle works one ticket.
func (a *Agent) Handle(ctx context.Context, t ticket.Ticket) (workflow.Outcome, string, error) {
	if a.repo.URL == "" {
		return workflow.OutcomeFailed, "", errors.New(a.role + ": no repository configured")
	}

	sb, err := a.boxes.Acquire(ctx, forge.SandboxSpec{
		Image: a.repo.Image, RunnerClass: a.repo.RunnerClass, TimeoutSecs: a.repo.TimeoutSecs,
		CloneURL: a.repo.URL, SecretRef: a.repo.SecretRef, Branch: a.repo.Branch,
		IdleTimeoutSecs: a.repo.LeaseIdleSecs, MaxLifetimeSecs: a.repo.LeaseMaxSecs,
	})
	if err != nil {
		return workflow.OutcomeFailed, "", fmt.Errorf("%s: %w", a.role, err)
	}
	// RELEASED ON EVERY EXIT PATH, INCLUDING THE PANICKING ONE. A held sandbox is
	// a runner class's worth of memory, and the timeouts that would reclaim it are
	// a backstop for a crashed agent, not a substitute for a stage that finished.
	defer sb.Release(ctx)

	branch := a.BranchFor(t)
	sb.AdoptBranch(branch)
	sb.AlsoMerge(MergeIntegrationScript(a.repo.IntegrationBranch))

	state := &State{
		Read: map[string]string{}, Staged: map[string]string{},
		Missing: map[string]bool{}, Baseline: map[string]string{},
		Budget: a.maxTurns,
	}

	tree, err := a.survey(ctx, sb)
	if err != nil {
		return workflow.OutcomeFailed, "", fmt.Errorf("%s: survey %s: %w", a.role, branch, err)
	}
	state.Tree = tree

	why := Reasons{
		Returned:   record.LatestMarked(t, record.ReturnedMarker),
		SpecRepair: record.LatestMarked(t, record.SpecRepairMarker),
	}
	return a.loop(ctx, t, why, state, sb, branch)
}

// loop runs the turns.
func (a *Agent) loop(
	ctx context.Context, t ticket.Ticket, why Reasons,
	s *State, sb Box, branch string,
) (workflow.Outcome, string, error) {
	for s.Iteration = 1; ; s.Iteration++ {
		// THE BUDGET INCLUDES THE REFUNDS, and the prompt has to show the same
		// number the loop enforces.
		s.Budget = Budget(a.maxTurns, s.Refunded)
		if why, done := s.Exhausted(a.deadCeiling()); done {
			return a.giveUp(ctx, t, s, why)
		}

		// THE MEMORY IS CLEARED AND THE WORK IS KEPT. See ResetAgent.
		if s.DueForReset() {
			s.ResetAgent(RestartBrief(a.mode, sortedKeys(s.Staged), s.Resets+1))
		}

		res, err := a.gw.Chat(ctx, a.class, BuildRequest(t, why, s, a.mode, a.prompt, a.tools))
		if err != nil {
			return workflow.OutcomeFailed, "", fmt.Errorf("%s: %w", a.role, err)
		}

		act, err := ReadReply(res, a.mode)
		if err != nil {
			s.ParseFails++
			s.Notice = ParseFailureNotice(res, err)
			if reason, done := s.StopParsing(); done {
				a.comment(ctx, t, fmt.Sprintf(
					"**%s stopped.** %s\n\nThe last error was:\n\n```\n%s\n```\n\n"+
						"and the reply it came from was:\n\n```\n%s\n```",
					a.role, reason, clip(err.Error(), 600), clip(res.Content, 800)))
				return workflow.OutcomeFailed, "unparseable model output", nil
			}
			continue
		}
		s.ParseFails = 0

		verify, applyErr := s.Advance(act, a.mode)
		s.Remember(TurnRecord(act, res.Content), a.outcomeOf(ctx, sb, s, act, applyErr))

		if !verify {
			continue
		}
		if err := a.verify(ctx, sb, t, s, branch); err != nil {
			return workflow.OutcomeFailed, "", fmt.Errorf("%s: verify %s: %w", a.role, branch, err)
		}
		if s.Finished() {
			return a.finish(ctx, t, s, branch)
		}
	}
}

// outcomeOf performs the side effect an action needs and describes what came of
// it, for the history line.
//
// THE READ HAPPENS HERE because it is the one action whose result the agent has
// to see: a write is described by what it staged, and an undo by what it
// restored, but a read is only useful once its contents are in the state.
func (a *Agent) outcomeOf(
	ctx context.Context, sb Box, s *State, act Action, applyErr error,
) string {
	switch {
	case applyErr != nil:
		return "refused: " + clip(applyErr.Error(), 200)

	case act.Action == ActionReadFiles:
		plan := s.PlanRead(act.Paths)
		if plan.Stale {
			s.StaleReads++
			s.Notice = s.StaleReadNotice(act.Paths)
			return "already had those files; the turn was refunded"
		}
		s.StaleReads = 0

		got, err := a.read(ctx, sb, plan.Fresh)
		if err != nil {
			s.NoProgress("Could not read those files: " + err.Error())
			return "the read failed: " + clip(err.Error(), 200)
		}
		s.RecordRead(plan.Fresh, got)
		return ReadOutcome(plan.Fresh, got)

	case act.Action == ActionWriteFiles:
		if s.NoopEdits > 0 {
			return "changed nothing; the file already held that text"
		}
		return fmt.Sprintf("staged %d file(s)", len(s.Staged))

	default:
		return clip(s.Notice, 200)
	}
}

func (a *Agent) survey(ctx context.Context, sb Box) ([]string, error) {
	res, err := sb.Run(ctx, recorderFrom(ctx), SurveyScript())
	if err != nil {
		return nil, err
	}
	if !res.OK() {
		return nil, fmt.Errorf("exit %d: %s", res.ExitCode, clip(res.Stderr+res.Stdout, 500))
	}
	return ParseSurvey(res.Stdout), nil
}

func (a *Agent) read(ctx context.Context, sb Box, paths []string) (map[string]string, error) {
	res, err := sb.Run(ctx, recorderFrom(ctx), ReadScript(paths))
	if err != nil {
		return nil, err
	}
	if !res.OK() {
		return nil, fmt.Errorf("exit %d: %s", res.ExitCode, clip(res.Stderr, 500))
	}
	return ParseRead(res.Stdout), nil
}

// verify pushes what the agent wrote and runs this stage's checks against the
// BRANCH.
//
// PUSHED FIRST, ALWAYS. The checks run on a fresh checkout of what was pushed,
// so a verification against an unpushed tree would report on something no other
// stage can see — which is how a developer came to be shown its own correct
// main.go while the gate said func main was undeclared.
func (a *Agent) verify(ctx context.Context, sb Box, t ticket.Ticket, s *State, branch string) error {
	push := PushScript(t, branch, s.Staged, s.Summary, s.CommitType, PushOptions{
		IntegrationBranch: a.repo.IntegrationBranch,
		FormatCommand:     a.repo.FormatCommand,
		DepsCommand:       a.repo.DepsCommand,
	})
	if res, err := sb.Run(ctx, recorderFrom(ctx), push); err != nil {
		return err
	} else if !res.OK() {
		// A PUSH THAT FAILED IS THE AGENT'S PROBLEM TO SEE, not the stage's to
		// die on: a rejected commit hook is something it can act on.
		s.RecordVerification("the branch could not be pushed:\n"+
			clip(gate.StripToolChatter(res.Stdout+res.Stderr), MaxTestOutput), false)
		return nil
	}

	res, err := sb.RunOnBranch(ctx, recorderFrom(ctx), branch, a.checkScript())
	if err != nil {
		return err
	}
	s.RecordVerification(clip(gate.StripToolChatter(res.Stdout+res.Stderr), MaxTestOutput), res.OK())
	return nil
}

// MaxTestOutput bounds what a verification puts back in the prompt. A suite with
// forty failures must not paste itself over the repository.
const MaxTestOutput = 8000

// checkScript is this stage's own definition of done.
func (a *Agent) checkScript() string {
	switch a.mode {
	case ModeTest:
		return gate.SpecScript(a.repo.TestCommand, false)
	case ModeSpecMerge:
		// REPAIRING RELAXES THE RED REQUIREMENT, NOT THE COMPILE CHECK. A merge
		// is a repair by definition: the sections were already red, and what is
		// being fixed is that they do not build together.
		return gate.SpecScript(a.repo.TestCommand, true)
	}
	return gate.Script(gate.Options{
		LintCommand:     a.repo.LintCommand,
		TestCommand:     a.repo.TestCommand,
		CriticalCommand: a.repo.CriticalCommand,
	})
}

// finish records the branch and hands the work on.
func (a *Agent) finish(ctx context.Context, t ticket.Ticket, s *State, branch string) (workflow.Outcome, string, error) {
	a.comment(ctx, t, record.PublishBranch(a.mode.BranchMarker(), branch)+"\n\n"+
		fmt.Sprintf("%s\n\nChanged %d file(s) in %d turns.",
			clip(s.Summary, MaxSummaryRunes), len(s.Staged), s.Iteration))
	return workflow.OutcomeSuccess,
		fmt.Sprintf("%d file(s) in %d turns", len(s.Staged), s.Iteration), nil
}

// giveUp reports an attempt that ran out.
//
// IT SAYS WHAT THE ATTEMPT ACTUALLY DID. An exhausted attempt otherwise reports
// only its last test output, and one that ended on a write has none at all — so
// the ticket says "stopped after 8 iterations" and nothing else.
func (a *Agent) giveUp(ctx context.Context, t ticket.Ticket, s *State, why string) (workflow.Outcome, string, error) {
	a.comment(ctx, t, fmt.Sprintf(
		"**%s stopped.** It %s.\n\nWhat it did: %s\n\nLast verification:\n\n```\n%s\n```",
		a.role, why, TrailSummary(s.Trail), clip(s.LastTest, 2000)))
	return workflow.OutcomeFailed, why, nil
}

func (a *Agent) deadCeiling() int { return MaxDeadRefusals }

func (a *Agent) comment(ctx context.Context, t ticket.Ticket, body string) {
	if a.store == nil {
		return
	}
	if _, err := a.store.AddComment(ctx, t.ID, body); err != nil {
		slog.ErrorContext(ctx, "could not write the stage's comment; its work is invisible on the ticket",
			"ticket_id", t.ID, "role", a.role, "error", err)
	}
}

// recorderFrom is the transcript this attempt is being written to.
//
// THE DEV LOOP RECORDED NO ACTIONS AT ALL until this existed: all four sandbox
// calls passed nil. It serves four of the eleven stages — the developer, the
// specification author, the coverage author and the spec merger — so more than
// a third of the board could never say what it was doing.
func recorderFrom(ctx context.Context) *transcript.Recorder {
	return transcript.RecorderFrom(ctx)
}
