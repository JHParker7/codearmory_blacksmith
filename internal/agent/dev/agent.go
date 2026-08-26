package dev

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/agent/referee"
	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/edit"
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

	// ref judges whether a specification can be satisfied at all. NIL IS A VALID
	// CONFIGURATION — a host with no second opinion still develops, it just does
	// not get the preflight — so every use is guarded rather than assumed.
	ref Referee
}

// Referee is the second opinion, in both the places it is worth having one.
//
// TWO QUESTIONS, ASKED AT DIFFERENT TIMES. Preflight reads the tests alone and
// asks whether any implementation could satisfy them — before a turn is spent.
// It can only catch what is visible in the tests by themselves.
//
// Judge is asked LATER, with the failing output and both sides in front of it,
// and it catches what preflight cannot: the case this package exists for is a
// test that builds its own local value and calls code reading a package-level
// one. That compiles, nothing panics, and the assertions simply fail against
// state the code cannot reach — there is no pattern to match, only a judgement
// about two files read together. Measured: 25 verification runs spent on one
// such specification.
//
// An interface rather than the concrete referee so this package does not depend
// on it, and so a test can supply a verdict without a model.
type Referee interface {
	Preflight(ctx context.Context, rec *transcript.Recorder, t ticket.Ticket,
		tests map[string]string) *referee.Verdict

	Judge(ctx context.Context, rec *transcript.Recorder, t ticket.Ticket,
		read map[string]string, out string) *referee.Verdict
}

// Options configure one stage built on this loop.
type Options struct {
	Mode         Mode
	Role         string
	SystemPrompt string
	MaxTurns     int
	Tools        bool

	// Referee judges an unsatisfiable specification before the developer spends
	// its budget proving it, and again once it has failed enough times to be
	// worth a second opinion. Optional.
	Referee Referee
}

// New builds a stage.
func New(gw Gateway, boxes Sandboxes, store Store, class model.Class, repo config.Repo, o Options) *Agent {
	turns := o.MaxTurns
	if turns <= 0 {
		turns = DefaultMaxIterations
	}
	// THE BRIEF COMES FROM THE MODE UNLESS ONE IS GIVEN.
	//
	// It used to come only from Options, and nothing in the department ever set
	// it — so every stage this loop serves ran with an EMPTY system message. A
	// field a caller must remember is a field some caller will forget; deriving
	// it means the omission cannot be expressed.
	prompt := o.SystemPrompt
	if strings.TrimSpace(prompt) == "" {
		prompt = SystemPromptFor(o.Mode, repo.CoverageTarget)
	}

	return &Agent{
		gw: gw, boxes: boxes, store: store, class: class, repo: repo,
		mode: o.Mode, role: o.Role, prompt: prompt, maxTurns: turns, tools: o.Tools,
		ref: o.Referee,
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

	// BEFORE THE FIRST TURN, because the ordinary route needs several failed
	// verifications first and by then the attempts are spent. A specification the
	// developer cannot satisfy costs one model call to notice here, against a
	// whole budget to prove by flailing.
	if outcome, detail, done := a.preflight(ctx, sb, t, state); done {
		return outcome, detail, nil
	}

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
		// BEFORE THE CEILINGS. An unsatisfiable specification is not an attempt
		// that ran out — it is one that could never have succeeded, and saying so
		// sends the ticket to the agent that can fix it rather than to a person
		// with "ran to 200 turns".
		a.consultReferee(ctx, t, s)
		if outcome, detail, done := a.endOnBrokenSpec(ctx, t, s); done {
			return outcome, detail, nil
		}
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
		// A REFUSAL IS EVIDENCE TOO. An agent being told it may not edit a test is
		// not working, so it may never reach another verification to be judged at.
		if applyErr != nil && edit.IsTestFile(firstEditPath(act)) {
			s.NoteTestEditRefusal(a.mode, firstEditPath(act))
		}
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

	case act.Action == ActionWriteFile, act.Action == ActionWriteFiles:
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
			clip(gate.StripToolChatter(res.Stdout+res.Stderr), MaxTestOutput), false, a.mode)
		return nil
	}

	res, err := sb.RunOnBranch(ctx, recorderFrom(ctx), branch, a.checkScript())
	if err != nil {
		return err
	}
	s.RecordVerification(clip(gate.StripToolChatter(res.Stdout+res.Stderr), MaxTestOutput),
		res.OK(), a.mode)
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

// preflight asks whether the tests can be satisfied at all, before any turn is
// spent on them.
//
// SKIPPED UNLESS THERE IS SOMETHING TO JUDGE: only the developer is bound by the
// tests it is given, only a tree with a test file has any, and a host with no
// referee simply does not make the check.
//
// THE READ IS NOT WASTED. The developer opens on the tests every time — it
// cannot satisfy them without reading them — so the contents are recorded as its
// first read rather than fetched again, and the sandbox is touched once.
func (a *Agent) preflight(
	ctx context.Context, sb Box, t ticket.Ticket, s *State,
) (workflow.Outcome, string, bool) {
	if a.ref == nil || a.mode != ModeDevelop {
		return "", "", false
	}
	var paths []string
	for _, p := range s.Tree {
		if edit.IsTestFile(p) {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return "", "", false
	}
	if len(paths) > MaxReadPaths {
		paths = paths[:MaxReadPaths]
	}

	tests, err := a.read(ctx, sb, paths)
	if err != nil {
		// A FAILED READ IS NOT A VERDICT. The developer will read them itself and
		// the ordinary routes still apply; refusing to start over a second opinion
		// that could not be gathered would be worse than not having one.
		return "", "", false
	}
	s.RecordRead(paths, tests)

	if v := a.ref.Preflight(ctx, recorderFrom(ctx), t, tests); v.Blames(referee.OwnerSpec) {
		s.SpecBroken = v.Reason
		return a.endOnBrokenSpec(ctx, t, s)
	}
	return "", "", false
}

// RefereeAfterFailures is how many red verifications an attempt takes before the
// referee is asked whose fault they are.
//
// THREE, because the question needs evidence and the first failure is not
// evidence of anything: test-first work is RED BY DESIGN at the start, so asking
// after one would put the referee on every ticket at its most misleading moment.
// By three the developer has tried and failed to move it, which is the shape the
// referee can actually read.
//
// It is also the number the deterministic matcher has had its chances at. The
// certain cases — tests that will not parse, an undefined symbol on its own —
// are decided without an opinion, and what is left by three is the ambiguous
// middle this exists for.
const RefereeAfterFailures = 3

// RefereeOnRecurrence is how many times one failure must be SEEN before the
// question of whose fault it is becomes worth buying an opinion on.
//
// SIGHTINGS, NOT CONSECUTIVE REPEATS. The expensive shape is a cycle: run 21's
// developer alternated between two routing schemes every thirty seconds, run
// 50's between a heading-order failure and a missing-form failure, each fix
// re-breaking the other. Neither ever repeated a failure twice in a row, so a
// consecutive counter read both as steady progress.
//
// THREE, because two is ordinary. A test that fails, is addressed, and fails
// once more is a normal edit-and-recheck. A third sighting means two attempts
// at it have not moved it.
// ONE, WHICH MEANS EVERY BEHAVIOURAL FAILURE. It was three, and waiting for a
// fault to come back three times has now failed twice for reasons that had
// nothing to do with the idea: the fingerprint counted handlers.go:67 and :68 as
// different faults (1da8996), then counted the same failure at 0.519s and 0.402s
// as different faults (14e8c6a). Both times the mechanism was right, the
// fingerprint was subtly wrong, and a loop ran to exhaustion while the thing
// built to stop it sat idle.
//
// THE ECONOMICS CHANGED WHEN THE VERDICT BECAME ADVICE. Run 50's ten consults
// were waste — every one said "dev" and every one was discarded, which is what
// the counting was for. Since 1da8996 the reason reaches the developer's prompt,
// and run 61 showed it acted on: three attempts at exactly the fix the referee
// described. A verdict is no longer a call spent to learn nothing.
//
// Against that, a loop costs minutes: run 21 ran 45 minutes, run 94 oscillated
// 36 turns on one file, run 93 43 turns. A consult costs one model call. Buying
// a diagnosis on every failure that a compiler cannot attribute is the cheaper
// side of that trade, and it removes the fingerprint from the critical path.
const RefereeOnRecurrence = 1

// MaxRefereeAsks bounds the second opinion for the whole attempt, because the
// referee is a large-model call and a stalled developer would otherwise buy one
// every eight turns until the ceiling.
// THE CEILING IS A RUNAWAY GUARD, NOT THE MECHANISM. It was three, which made
// it the thing that actually limited consultation; the compile-error filter is
// what should do that, because those failures already have a mechanical answer.
// Set high enough not to bind on a healthy attempt and low enough that a
// pathological one cannot spend the slot indefinitely.
const MaxRefereeAsks = 30

// consultReferee asks whose fault the failures are, once there have been enough
// of them to be worth asking about, and AGAIN if the attempt later stalls.
//
// IT USED TO ASK EXACTLY ONCE, on the grounds that the specification does not
// change while the developer works. That is true and it is not the point: which
// part of the specification the developer is stuck against changes completely.
// Measured on run 21 — the referee was asked 90 seconds in, correctly answered
// "dev" because there was a compile error in board.go, and that verdict was
// frozen for the next 70 turns while the failure became an assertion no
// implementation could satisfy. Nothing asked again and the attempt ran to the
// 45-minute wall clock.
//
// SO THERE ARE TWO TRIGGERS, and they are deliberately different questions.
// FailedVerifications asks "has this gone wrong enough to be worth a look" and
// fires early. SameFailure asks "has this stopped moving", which is the shape
// an unsatisfiable target actually makes, and can only fire late.
//
// IT DEFERS TO THE DETERMINISTIC ROUTE. If the matcher has already decided the
// specification is broken there is nothing to arbitrate, and this does not run.
//
// The verdict is written into the SAME field the matcher sets, so everything
// downstream — the bounded hand-backs, the note that quotes the evidence, the
// stop for a person — is the one path rather than a second one that has to be
// kept in step with it.
func (a *Agent) consultReferee(ctx context.Context, t ticket.Ticket, s *State) {
	if a.ref == nil || a.mode != ModeDevelop {
		return
	}
	if s.SpecBroken != "" || s.RefereeAsks >= MaxRefereeAsks {
		return
	}
	// A COMPILE ERROR IS NOT AN OPEN QUESTION. The matcher attributes it without
	// an opinion: a non-test file is the developer's, an undefined symbol is the
	// expected red of test-first, a fault local to a test file is the author's.
	// Asking here buys a verdict for something already decided — on run 50 that
	// was eight of ten consults, every one of them "dev".
	if HasCompileErrors(s.LastTest) {
		return
	}
	// WHAT IS LEFT IS A SUITE THAT BUILDS AND FAILS AN ASSERTION, which no
	// mechanical route can attribute — and it is only worth asking about once the
	// developer has stopped moving it. A failing test that KEEPS CHANGING is
	// progress; one that keeps coming back is not.
	if s.FailureSightings[FailureFingerprint(s.LastTest)] < RefereeOnRecurrence {
		return
	}
	// Counted before the call, not after: a call that fails is still a call
	// spent, and retrying it every iteration is how one unavailable referee
	// becomes a hundred requests.
	s.RefereeAsks++
	// THE SIGHTINGS RESTART WITH THE QUESTION. Without this the condition stays
	// true and every remaining turn buys another verdict on the same evidence,
	// which is the runaway the ask ceiling would then have to absorb. The failure
	// must come back RefereeOnRecurrence times again to be worth asking twice.
	s.FailureSightings[FailureFingerprint(s.LastTest)] = 0

	// IT CONVICTS BUT DOES NOT ACQUIT, and that asymmetry is deliberate. The guard
	// above means this never runs while a verdict stands, because a compile fault
	// local to a test file is conclusive on sight and there is nothing to
	// arbitrate. Letting a second opinion overturn that would put a sampler's bad
	// day between a broken specification and the agent that can fix it.
	//
	// What made the standing verdict dangerous was that it outlived its evidence,
	// and that is fixed where it was caused — JudgeSpec now clears when the
	// failure leaves the test files. An acquittal here would have been a second
	// mechanism aimed at the same defect.
	v := a.ref.Judge(ctx, recorderFrom(ctx), t, s.Read, s.LastTest)
	if v.Blames(referee.OwnerSpec) {
		s.SpecBroken = v.Reason
		return
	}
	// AND A VERDICT THAT DOES NOT HAND THE TICKET BACK IS STILL WORTH READING.
	//
	// It used to be discarded. Run 50 bought ten diagnoses of this kind — "pass
	// non-pointer values to errors.As, which requires a pointer to a type that
	// implements error", naming the file and the lines — and threw every one
	// away, while the developer went on failing the same way. The agent that has
	// to act on the failure is the one agent that never saw the analysis of it.
	if v != nil && strings.TrimSpace(v.Reason) != "" {
		s.Hint = v.Reason
	}
}

// Brief is the system prompt this stage runs under.
//
// EXPORTED SO THE ASSEMBLY CAN BE CHECKED. The loop ran with an empty brief for
// as long as it did because every test built its own agent, with the same gap
// the department had — so the tests exercised the misconfiguration and passed.
// Nothing could ask an assembled stage what it was actually configured with.
func (a *Agent) Brief() string { return a.prompt }

// Mode is which job this stage does, for the same reason.
func (a *Agent) ModeName() Mode { return a.mode }

// SpecRepairsSoFar counts how many times this ticket has already been handed
// back to its author.
func SpecRepairsSoFar(t ticket.Ticket) int {
	var n int
	for _, c := range t.Comments {
		if strings.Contains(c.Body, record.SpecRepairMarker) {
			n++
		}
	}
	return n
}

// endOnBrokenSpec ends the attempt when the specification has been judged
// unsatisfiable: back to its author while there is budget for that, and stopping
// for a person once there is not.
//
// BACK TO THE AUTHOR FIRST. Stopping dead treats an unsatisfiable specification
// as a decision to make, and that holds for a contradiction. It does not hold
// for what actually arrives, which is a mechanical defect — a helper missing its
// *testing.T, an import left out, a symbol declared twice — and the one agent
// allowed to fix it is never asked.
//
// Bounded, because an author that cannot fix its own tests twice will not manage
// it on a third pass, and then a person really is the right answer.
//
// THE NOTE IS THE POINT. It goes back with the fault named and the output
// quoted, because the author is about to be asked to fix something it cannot
// reproduce — it does not run the implementation, and without the evidence it
// is being told only that someone was unhappy.
func (a *Agent) endOnBrokenSpec(
	ctx context.Context, t ticket.Ticket, s *State,
) (workflow.Outcome, string, bool) {
	if s.SpecBroken == "" {
		return "", "", false
	}

	repairs := SpecRepairsSoFar(t)
	if repairs < MaxSpecRepairs {
		a.comment(ctx, t, fmt.Sprintf(
			"%s\n\n**What is wrong with `%s`: %s.**\n\nThis stage may not edit test "+
				"files, so no change to the implementation could make them pass. The fault "+
				"is in the specification, not in the code.\n\nGoing back to the agent that "+
				"CAN correct it. Repair attempt %d of %d.\n\nWhat it tried: %s"+
				"\n\nLast verification:\n\n```\n%s\n```",
			record.SpecRepairMarker, s.SpecBroken, s.FaultOrDefault(), repairs+1,
			MaxSpecRepairs, TrailSummary(s.Trail), clip(s.LastTest, 2000)))
		return workflow.OutcomeReturned,
			"specification is unsatisfiable; returned to its author", true
	}

	a.comment(ctx, t, fmt.Sprintf(
		"%s\n\n**What is wrong with `%s`: %s.**\n\nThis stage may not edit test files, "+
			"so no change to the implementation can make them pass.\n\nIt has been sent "+
			"back to its author %d times and still cannot be, so a person needs to correct "+
			"the tests or the ticket needs re-scoping.\n\nWhat it tried: %s"+
			"\n\nLast verification:\n\n```\n%s\n```",
		record.BrokenSpecMarker, s.SpecBroken, s.FaultOrDefault(), MaxSpecRepairs,
		TrailSummary(s.Trail), clip(s.LastTest, 2000)))
	return workflow.OutcomeBlocked, "specification is unsatisfiable", true
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

// firstEditPath names the file an action tried to write, or "" when it wrote
// nothing. Used to tell a refused test-file edit from any other refusal.
func firstEditPath(act Action) string {
	if len(act.Edits) == 0 {
		return ""
	}
	return act.Edits[0].Path
}
