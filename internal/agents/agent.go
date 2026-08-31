// Package agents creates agents and runs them.
//
// It holds the ONLY calls to the model in the rebuilt pipeline. Everything a
// stage does that is not "ask the model something" is a tool call, and the tools
// live in internal/tools; what remains here is a loop, a budget, and one
// constructor per stage.
//
// CREATION IS THE ABSTRACTION. A Creator holds the wiring every agent on a host
// shares — the gateway, the sandbox, where the log goes — so building a stage
// says only what makes that stage different: its instruction, what it may write,
// which tools it gets, and what decides it is done. Assembling a workspace, a
// tool set and a loop by hand at each call site is how those three drift out of
// agreement, and the guard is the one of them that must not.
package agents

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/tools"
)

// Gateway is the model, narrowed to the one method a stage uses.
//
// AN INTERFACE, so a test can run the whole loop without an inference server.
// The concrete gateway is internal/model.Gateway.
type Gateway interface {
	Chat(ctx context.Context, class model.Class, req model.ChatRequest) (model.ChatResult, error)
}

// Creator builds agents that share a host's wiring.
//
// The zero value is not useful: Gateway is required, and an agent whose stage
// has a check needs a Sandbox to run it in.
type Creator struct {
	Gateway Gateway

	// Sandbox is where a check runs. Nil is allowed and only matters for the
	// stages that have one — the reviewer and the document stages never touch it.
	Sandbox tools.Sandbox

	// Check overrides every stage's default check command.
	//
	// "Does this work" is a property of the REPOSITORY, not of the stage looking
	// at it. The defaults exist so the pipeline runs on a host that configured
	// nothing; this is what an operator sets when their tree is not built the way
	// the default assumes.
	Check string

	// Log receives one line per tool call. Nil is fine. Present because a stage
	// that takes twenty minutes in silence is indistinguishable from a hung one.
	Log func(string)

	// OnWrite hears every landed edit, for the caller that turns writes into
	// commits. Shared by every stage a creator builds, because the history is a
	// property of the RUN, not of any one stage.
	OnWrite func(path, content string, deleted bool, message string)

	// FileTicket files a finding on the board, for the stages offered the tool.
	FileTicket func(kind, title, body, severity string) (string, error)

	// MergeFix merges an approved fix into dev, for the review stage.
	MergeFix func(reason string) (string, error)
}

// Options is what makes one stage different from another.
//
// Everything here is per-stage. Anything shared between stages belongs on the
// Creator, and the test for which is whether two stages could sensibly disagree
// about it.
type Options struct {
	// Name appears in logs and errors. It is also how -roles addresses a stage,
	// so it is part of the interface rather than decoration.
	Name string

	// Class is the serving class this stage asks for. Which model that is, and
	// where it runs, is the gateway's business and not the stage's.
	Class model.Class

	// Prompt is the standing instruction: what this stage IS. What a particular
	// piece of work is goes to Run instead.
	Prompt string

	// Guard decides what this stage may write. The strongest statement of what a
	// stage is for, and the only one a model cannot talk its way past.
	Guard tools.Guard

	// Tools limits what is offered. Empty offers everything, which is almost
	// never what a stage wants.
	Tools []string

	// TicketKind labels the findings a reviewing stage files: "security" or
	// "quality". Empty for stages that file none.
	TicketKind string

	// Check is the command that decides whether this stage succeeded. Empty means
	// the stage ends when the model stops calling tools — right for a stage whose
	// product is prose, wrong for every stage that writes code.
	Check string

	// RewriteWhole lets this stage replace implementation files wholesale, drop
	// check and all. SOUND ONLY WITH A LOCKED SUITE: the stage's tests must be
	// someone else's to write and not this stage's to touch, so that a dropped
	// function fails the next check by name instead of vanishing silently.
	RewriteWhole bool

	// AttemptTimeout bounds ONE ATTEMPT at this stage in wall-clock time, and
	// Respins is how many fresh starts follow a timeout. Zero timeout means
	// unbounded; zero respins means a timeout simply fails the stage.
	//
	// THE RESPIN THROWS AWAY THE TRAIL AND KEEPS THE TREE. A wedged agent's
	// accumulated context is part of the wedge — the trail full of identical
	// checks is what makes the next identical check the most probable move —
	// while the tree holds all the actual work. A fresh agent inheriting the
	// files sees a red suite with clean eyes. Measured on the run that bought
	// this: ten minutes of a dev re-running an unchanging check, each run
	// resetting the stall counter (its output never byte-identical thanks to
	// test timings), no bound anywhere that could fire.
	AttemptTimeout time.Duration
	Respins        int

	// OwnCheck exempts this stage from the Creator's check override.
	//
	// The override is the operator saying what "the tests pass" means in their
	// repository — and the specification stage's check means the OPPOSITE: green
	// when the tests fail. Substituting the operator's command there would
	// quietly re-invert the one stage whose gate points the other way.
	OwnCheck bool

	MaxIterations int
	Temperature   float64
	MaxTokens     int

	// SeedKnown shows the whole tree in the first prompt instead of making the
	// agent read files in one by one. For a REVIEWER, which judges code it did
	// not write, this is the difference between the department reviewer's single
	// handed-over diff and a 25-turn discovery loop.
	SeedKnown bool
}

// An Agent is one stage, wired and ready to run.
//
// It OWNS its workspace, so the tree it produces is read back from it rather
// than from a workspace the caller built and kept a handle on. Those two
// arrangements look the same until a guard is attached to one of them.
type Agent struct {
	opts    Options
	tools   *tools.Set
	space   *tools.Workspace
	gateway Gateway
	log     func(string)
}

// New builds an agent over a snapshot of a file tree.
func (c Creator) New(files map[string]string, o Options) *Agent {
	if o.Guard == nil {
		// A stage with no guard may write anything, and a nil guard is far more
		// likely to be an omission than an intention. Refusing to infer "write
		// anything" from silence costs a caller one explicit tools.AllowAll.
		o.Guard = tools.DenyAll
	}
	space := tools.NewWorkspace(files, o.Guard)
	if o.RewriteWhole {
		space.AllowWholeRewrites()
	}
	if o.SeedKnown {
		space.SeedKnown()
	}
	return &Agent{
		opts:  o,
		space: space,
		tools: &tools.Set{
			Workspace:  space,
			Sandbox:    c.Sandbox,
			Check:      c.checkFor(o),
			Names:      o.Tools,
			OnWrite:    c.OnWrite,
			FileTicket: c.FileTicket,
			TicketKind: o.TicketKind,
			MergeFix:   c.MergeFix,
		},
		gateway: c.Gateway,
		log:     c.Log,
	}
}

// checkFor resolves the command a stage gates on, preferring what the operator
// configured for this repository over the stage's own default.
func (c Creator) checkFor(o Options) string {
	if o.Check == "" || o.OwnCheck {
		return o.Check
	}
	if c.Check != "" {
		return c.Check
	}
	return o.Check
}

// Name is the stage this agent runs.
func (a *Agent) Name() string { return a.opts.Name }

// AttemptTimeout and Respins expose the stage's wall-clock bound, for the
// driver that enforces it — the loop itself cannot, because killing an attempt
// and building its successor is the caller's business.
func (a *Agent) AttemptTimeout() time.Duration { return a.opts.AttemptTimeout }
func (a *Agent) Respins() int                  { return a.opts.Respins }

// Class is the serving class this agent will ask for.
func (a *Agent) Class() model.Class { return a.opts.Class }

// Check is the command this agent gates on, resolved against any operator
// override, or empty when the stage ends on its answer instead.
//
// Exposed so a caller can report the wiring — which class, which command —
// WITHOUT running anything. A host that cannot serve a stage should say so
// before it spends an hour discovering it at the fifth one.
func (a *Agent) Check() string { return a.tools.Check }

// Files is the tree as the agent left it.
func (a *Agent) Files() map[string]string { return a.space.Files() }

// Offers reports the tools this agent may call, for a caller that wants to check
// the wiring without running anything.
func (a *Agent) Offers() []model.Tool { return a.tools.Definitions() }

// A Step is one thing the agent did, kept for the trail.
type Step struct {
	Tool   string
	Args   string
	Result string
}

// Outcome is how a stage ended.
type Outcome struct {
	// Passed reports that the stage's check exited zero. A stage with no check
	// passes when the model stops calling tools.
	Passed bool

	Iterations int

	// Answer is the model's final prose, when it ended by answering rather than
	// by its check going green.
	Answer string

	// LastCheck is the output of the last check that ran, empty if none did. The
	// caller needs it: a stage that ends unpassed is only actionable alongside
	// what its check said.
	LastCheck string

	// Stalled reports that the stage was stopped for looping rather than for
	// running out of budget.
	//
	// KEPT APART FROM "ran out of budget", because they need opposite responses.
	// A stage that spent its budget working may deserve a larger one; a stage
	// that stopped moving would do exactly the same thing with twice as many
	// turns, and the fix is in the prompt or the tools rather than the number.
	Stalled bool

	Trail []Step
}

// Run works the task until the stage's check passes or its budget runs out.
//
// THE PROMPT IS REBUILT EVERY TURN rather than accumulated as a conversation.
// That is this department's convention and it is load-bearing twice over: the
// prompt stays bounded no matter how long the stage runs, and a thinking model's
// scratchpad never gets replayed to it as though it were part of the dialogue,
// which is how one talks itself into a loop. What carries between turns is the
// trail — what was tried and what came back — rendered fresh each time.
func (a *Agent) Run(ctx context.Context, task string) (Outcome, error) {
	out := Outcome{}
	idle := 0

	// produced counts everything this stage delivered — accepted writes AND
	// filed tickets. The security reviewer writes nothing to the tree; its
	// deliverable is tickets on the board, so gating completion on tree writes
	// alone reported a reviewer that filed seven findings as having done
	// nothing, failed the run, and rerolled it three times over a stage that
	// succeeded every time.
	produced := 0

	for i := 0; i < a.opts.MaxIterations; i++ {
		out.Iterations = i + 1

		res, err := a.gateway.Chat(ctx, a.opts.Class, model.ChatRequest{
			Messages:    a.messages(task, out.Trail, out.LastCheck),
			Temperature: stuckTemperature(a.opts.Temperature, idle),
			MaxTokens:   a.opts.MaxTokens,
			Tools:       a.tools.Definitions(),
		})
		if err != nil {
			return out, fmt.Errorf("%s: turn %d: %w", a.opts.Name, out.Iterations, err)
		}

		// THE REASONING GOES IN THE LOG, and only in the log. The lesson is
		// written on ChatResult.Reasoning and was relearned here watching a live
		// run re-read one file five times: a stuck run can only be diagnosed from
		// what the agent DID, and that is not enough — the loop is visible and its
		// cause is not. Never into the trail or the prompt: replaying a thinking
		// model's scratchpad to it is how it talks itself into a loop.
		if r := res.Reasoning; r != "" {
			a.logf("%s: thinking: %s", a.opts.Name, reasoningLine(r))
		}

		// NO TOOL CALL MEANS IT ANSWERED — but an answer only finishes a stage
		// that has nothing else outstanding, and there are three ways to owe more:
		//
		//   - the stage has a CHECK, and the check is what decides;
		//   - the reply was TRUNCATED, so this is half an answer that stopped at a
		//     ceiling, not a finished one — accepting it ships half a plan;
		//   - the stage's deliverable is FILES (it was given write_file) and it has
		//     not written any. An architect whose plan exists only as prose printed
		//     to stdout hands the next stage an empty tree, which is the same
		//     empty-tree pass the stall path already refuses.
		//
		// Every refusal names the cause, and NONE of them dodges the idle counter:
		// answering in prose changes nothing, and a model that does it every turn
		// is exactly as stuck as one re-reading a file. The `continue` that used to
		// live here skipped the stall bound, so that model burned its whole budget
		// unbounded.
		if len(res.Calls) == 0 {
			truncated := res.Truncated(a.opts.MaxTokens)
			owesFiles := a.offersTool(tools.WriteFile) && produced == 0

			if a.opts.Check == "" && !truncated && !owesFiles {
				out.Answer = res.Content
				out.Passed = true
				a.logf("%s: finished after %d turns", a.opts.Name, out.Iterations)
				return out, nil
			}

			var notice string
			switch {
			case truncated:
				notice = "Your reply was CUT OFF at the token limit — it is not finished, it " +
					"ran out of room. Do not retry it at the same length: say less, or do the " +
					"work through tool calls instead of prose."
			case a.opts.Check != "":
				notice = "You answered in prose, but this stage ends when its check passes, not " +
					"when you say it is done. Call " + tools.RunCommand + " to run the check, or " +
					tools.WriteFile + " to change something first."
			default:
				notice = "You answered in prose, but this stage's deliverable is FILES and you " +
					"have not written any. Whatever you just said exists nowhere the next stage " +
					"can read it. Put it in a file with " + tools.WriteFile + "."
			}
			out.Trail = append(out.Trail, Step{
				Tool:   "(no tool call)",
				Args:   trim(res.Content, 400),
				Result: notice,
			})
		}

		var wrote, ranCheck bool
		for _, call := range res.Calls {
			result, err := a.tools.Invoke(ctx, call.Name, call.Arguments)
			if err != nil {
				// The sandbox is unreachable or similar. Not something the model can
				// reason its way out of, so it ends the stage rather than becoming a
				// refusal it would keep retrying.
				return out, fmt.Errorf("%s: %s: %w", a.opts.Name, call.Name, err)
			}
			// The ARGUMENTS ride in the log line, head first, because the head of a
			// write's JSON is its path. A stage refusing the same edit for fifteen
			// minutes was undiagnosable from the log alone: it showed the refusal
			// and never what was being refused.
			a.logf("%s: %s(%s) -> %s", a.opts.Name, call.Name, argHead(call.Arguments), firstLine(result))
			out.Trail = append(out.Trail, Step{
				Tool:   call.Name,
				Args:   trim(call.Arguments, 300),
				Result: result,
			})

			if progressed(call.Name, result) {
				idle = 0
				switch call.Name {
				case tools.WriteFile, tools.FileTicket, tools.MergeFix:
					wrote = true
					produced++
				case tools.UndoEdit:
					// An undo is progress — it is not a stuck loop — but it is the
					// opposite of production: write-then-undo nets to nothing, and a
					// stage that ends there has delivered nothing.
					wrote = true
					if produced > 0 {
						produced--
					}
				}
			}

			if call.Name == tools.RunCommand {
				ranCheck = true
				out.LastCheck = result
				if checkPassed(result) {
					out.Passed = true
					a.logf("%s: check passed after %d turns", a.opts.Name, out.Iterations)
					return out, nil
				}
			}

			// A SUCCESSFUL MERGE ENDS THE REVIEW. Once the reviewer approves and
			// the fix is on dev there is nothing left to review, so approval is
			// terminal the way a green check is — otherwise the model keeps
			// calling merge_fix, re-merging what is already merged. Measured on
			// the first live end-to-end: four merge_fix calls for one approval.
			if call.Name == tools.MergeFix && !strings.HasPrefix(result, "Error:") {
				out.Passed = true
				a.logf("%s: approved and merged after %d turns", a.opts.Name, out.Iterations)
				return out, nil
			}
		}

		// THE CHECK RUNS BY ITSELF after any turn that landed a write, and green
		// ENDS the stage. Waiting for the model to ask had two costs, both
		// measured: a stage that never asked spent its budget polishing a tree
		// that had been green for turns — the check is what says "done", and a
		// model left to decide when to hear it defers it — and a stage that asked
		// early and never again finished "budget spent, check never passed" with
		// working code on disk. One check per WRITING turn, not per write, so a
		// burst of files costs one execution; turns whose writes were all refused
		// run nothing, so a refusal loop cannot burn the sandbox.
		if wrote && !ranCheck && a.opts.Check != "" {
			result, err := a.tools.Invoke(ctx, tools.RunCommand, "{}")
			if err != nil {
				return out, fmt.Errorf("%s: auto-check: %w", a.opts.Name, err)
			}
			a.logf("%s: auto-check -> %s", a.opts.Name, firstLine(result))
			out.Trail = append(out.Trail, Step{Tool: tools.RunCommand, Args: "(auto)", Result: result})
			out.LastCheck = result
			if checkPassed(result) {
				out.Passed = true
				a.logf("%s: check passed after %d turns", a.opts.Name, out.Iterations)
				return out, nil
			}
		}

		if idle++; idle >= MaxIdleTurns {
			out.Stalled = true

			// A STAGE WITH NO CHECK IS DONE WHEN IT STOPS CHANGING THINGS.
			//
			// Its success condition is "produced its deliverable and stopped", and
			// the deliverable is the tree it leaves behind. Stopping by answering in
			// prose and stopping by running out of things to change are the same
			// event seen from two angles; only the first was being treated as
			// finishing. Measured: an architect wrote a 424-line plan and a 298-line
			// test plan, then circled without answering, and this reported the stage
			// FAILED and threw all 722 lines away.
			//
			// Stalled is still recorded, because "it stopped tidily" and "it went
			// round in circles until we stopped it" are worth telling apart in a
			// report even when both count as done.
			// ...BUT ONLY IF IT PRODUCED SOMETHING. A stage that stopped without
			// ever writing has not finished quietly, it has done nothing, and
			// calling that done hands the next stage an empty tree and a plan that
			// does not exist. Measured: an architect called list_files fifteen times
			// against an empty repository, wrote nothing, was reported as passed,
			// and the developer then started from nothing and stalled the same way.
			if a.opts.Check == "" && produced > 0 {
				out.Passed = true
				a.logf("%s: stopped after %d turns having delivered; taking that as its answer",
					a.opts.Name, out.Iterations)
				return out, nil
			}

			a.logf("%s: stopped after %d turns with nothing changed in the last %d",
				a.opts.Name, out.Iterations, idle)
			return out, nil
		}
	}

	// A CHECKLESS STAGE IS JUDGED THE SAME AT EVERY EXIT. The stall path already
	// takes a written deliverable as completion; the budget path fell through to
	// failure, so an architect that wrote its plan in one shot and then spent its
	// remaining turns re-reading it was reported failed WITH ITS DELIVERABLE ON
	// DISK — same situation as a stall, opposite verdict, and which one a run got
	// depended on nothing but whether the loop noticed the idleness first.
	if a.opts.Check == "" && produced > 0 {
		out.Passed = true
		a.logf("%s: budget spent after %d turns having delivered; taking that as its answer",
			a.opts.Name, out.Iterations)
		return out, nil
	}

	a.logf("%s: out of budget after %d turns", a.opts.Name, out.Iterations)
	return out, nil
}

// progressed reports whether a tool call changed anything.
//
// READING IS NOT PROGRESS, however much of it happens. Only a write that was
// accepted and a check that actually ran move a stage forward — everything else
// is the agent deciding what to do, which is necessary but cannot be the thing
// that keeps it alive.
//
// A REFUSED WRITE DOES NOT COUNT, and that is the whole point: an agent
// repeating an edit the guard rejects is exactly as stuck as one re-reading, and
// counting the attempt would hide it.
func progressed(name, result string) bool {
	switch name {
	case tools.WriteFile, tools.UndoEdit, tools.FileTicket, tools.MergeFix:
		return !strings.HasPrefix(result, "Error:")
	case tools.RunCommand:
		return true
	}
	return false
}

// offersTool reports whether this agent's set includes a tool, which is how the
// loop tells a stage whose deliverable is files (it can write them) from one
// whose deliverable is its answer (it cannot).
func (a *Agent) offersTool(name string) bool {
	for _, t := range a.tools.Definitions() {
		if t.Name == name {
			return true
		}
	}
	return false
}

func (a *Agent) logf(format string, args ...any) {
	if a.log != nil {
		a.log(fmt.Sprintf(format, args...))
	}
}

// MaxKnownChars bounds the file contents carried in a prompt.
//
// Generous, because this is the agent's memory of its own work and starving it
// is what this bound exists to prevent rather than cause. When the budget runs
// out the remaining files are LISTED BY NAME rather than truncated: a name tells
// the agent to go and read the file, while half a file tells it nothing is
// missing.
const MaxKnownChars = 60000

// messages renders the whole prompt for one turn.
func (a *Agent) messages(task string, trail []Step, lastCheck string) []model.Message {
	var b strings.Builder
	b.WriteString(task)

	// THE FILES COME BEFORE THE TRAIL, and they come in full.
	//
	// The prompt is rebuilt every turn, so whatever is not written here does not
	// exist as far as this turn is concerned. Reconstructing the tree from tool
	// results in the trail was the original design and it does not work: results
	// are trimmed, the window rolls, and a file the agent wrote twenty turns ago
	// silently becomes half a file. Served from the workspace instead, which is
	// the same bytes the edit tools resolve against — so what the agent is shown
	// and what it addresses cannot disagree.
	if known := a.space.Known(); len(known) > 0 {
		b.WriteString("\n\n--- the files you have read or written, as they are NOW ---\n")
		b.WriteString(a.renderKnown(known))
	}

	if len(trail) > 0 {
		b.WriteString("\n\n--- what you have done so far ---\n")
		b.WriteString(RenderTrail(trail))
	}
	if lastCheck != "" {
		// THE CHECK OUTPUT IS REPEATED IN FULL at the end, even though it is
		// already in the trail, and it is the one thing that gets that treatment.
		// It is what the next edit has to answer, and in the trail it is one entry
		// among many by the time it matters.
		b.WriteString("\n\n--- what the check last said ---\n")
		b.WriteString(trim(lastCheck, 6000))
	}

	return []model.Message{
		{Role: "system", Content: a.opts.Prompt},
		{Role: "user", Content: b.String()},
	}
}

// MaxIdleTurns is how many turns in a row may pass without the agent changing
// anything before the stage is stopped.
//
// A BUDGET BOUNDS THE WORK; THIS BOUNDS THE LOOPING, and they are different
// failures. A stage that spends 120 turns writing and re-checking is expensive
// and working. A stage that spends 120 turns reading is not working at all, and
// the turns it has left are the only thing keeping it alive.
//
// Measured on the first live run of this loop: 76 reads, 2 writes, run_command
// never called, and the same two files read to the end of the budget. Every one
// of those turns cost a model call. Stopping at the point where nothing has
// changed for a while turns a wasted budget into a report that says what
// happened.
//
// Generous on purpose. Reading several files before an edit is normal, and this
// must not fire on a careful agent — only on one that has stopped moving.
const MaxIdleTurns = 15

// renderKnown writes the current contents of the files the agent knows about,
// newest first, until the budget runs out.
//
// NEWEST FIRST because the file just written is the one the next action depends
// on, and if anything has to be dropped it should be the one touched longest
// ago. Numbered, because every edit refusal tells the agent to copy from "the
// numbered contents" and to address repeated lines by number.
func (a *Agent) renderKnown(known []string) string {
	var b strings.Builder
	var spent int
	var omitted []string

	for i := len(known) - 1; i >= 0; i-- {
		p := known[i]
		content, ok := a.space.Read(p)
		if !ok {
			continue
		}
		block := fmt.Sprintf("=== %s ===\n%s\n\n", p, numbered(content))

		// A FILE TOO BIG FOR THE WHOLE BUDGET cannot be handled by omission.
		// "Read it if you need it" is the omission remedy, and for this file it is
		// a trap: reading changes nothing, because the next render omits it again
		// — measured shape: an unbreakable loop of read, omit, stale-notice. So an
		// oversized file is shown head and tail with the ELIDED RANGE NAMED, which
		// is not the silent half-file this package refuses to serve elsewhere: the
		// agent is told exactly which lines it cannot see and how to address them.
		if len(block) > MaxKnownChars {
			block = fmt.Sprintf("=== %s ===\n%s\n\n", p, elided(content))
		}
		if spent+len(block) > MaxKnownChars {
			omitted = append(omitted, p)
			continue
		}
		spent += len(block)
		b.WriteString(block)
	}

	if len(omitted) > 0 {
		// NAMED, NOT TRUNCATED. A name is an instruction the agent can act on;
		// half a file reads as a whole one and is acted on as though nothing were
		// missing.
		fmt.Fprintf(&b, "(not shown, read them if you need them: %s)\n",
			strings.Join(omitted, ", "))
	}
	return b.String()
}

// ElidedHeadLines and ElidedTailLines shape the view of a file too large to
// show whole. The head carries the declarations and the tail the most recent
// growth; what is lost is the middle, and the elision names it.
const (
	ElidedHeadLines = 250
	ElidedTailLines = 120
)

// elided renders a file's head and tail with the missing range named.
func elided(content string) string {
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	head, tail := ElidedHeadLines, ElidedTailLines
	if len(lines) <= head+tail {
		return numbered(content)
	}
	width := len(fmt.Sprint(len(lines)))
	var b strings.Builder
	for i := 0; i < head; i++ {
		fmt.Fprintf(&b, "%*d\t%s\n", width, i+1, lines[i])
	}
	fmt.Fprintf(&b,
		"… lines %d-%d NOT SHOWN (%d lines): this file is too large to show whole. Address "+
			"edits in this range by \"start_line\"/\"end_line\" or \"decl\"; the numbering above "+
			"and below is real …\n",
		head+1, len(lines)-tail, len(lines)-head-tail)
	for i := len(lines) - tail; i < len(lines); i++ {
		fmt.Fprintf(&b, "%*d\t%s\n", width, i+1, lines[i])
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// numbered prefixes each line with its number, matching what read_files serves.
func numbered(text string) string {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	width := len(fmt.Sprint(len(lines)))
	var b strings.Builder
	for i, l := range lines {
		fmt.Fprintf(&b, "%*d\t%s\n", width, i+1, l)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// TrailWindow is how many past steps a turn is shown.
//
// A WINDOW RATHER THAN THE WHOLE HISTORY, because the prompt has to stay bounded
// across a stage that may run for a hundred turns. Recent steps are what a next
// action depends on; the tree itself is the record of everything earlier, and
// read_files reaches it.
const TrailWindow = 12

// RenderTrail formats the recent steps for the prompt.
func RenderTrail(trail []Step) string {
	from := 0
	if len(trail) > TrailWindow {
		from = len(trail) - TrailWindow
	}
	var b strings.Builder
	if from > 0 {
		fmt.Fprintf(&b, "(%d earlier steps not shown)\n", from)
	}
	for i, s := range trail[from:] {
		fmt.Fprintf(&b, "%d. %s %s\n   -> %s\n", from+i+1, s.Tool, s.Args, trim(s.Result, TrailResultChars))
	}
	return b.String()
}

// TrailResultChars is how much of a tool's result the trail carries.
//
// SHORT ON PURPOSE, now that the files are rendered in full above it. The trail
// is a record of WHAT WAS TRIED — the file contents it used to carry were a
// duplicate of the real ones and, being truncated, a misleading duplicate. Spend
// the budget on the files.
const TrailResultChars = 400

// checkPassed reads the exit status out of run_command's output.
//
// The format is this package's own — see tools.Set.Invoke — so this is parsing
// something we also write. Kept as a string rather than a typed result because
// the same text is what the model reads, and two representations of "did it
// pass" that can disagree is a bug waiting to happen.
func checkPassed(result string) bool {
	for _, line := range strings.Split(result, "\n") {
		if strings.HasPrefix(line, "exit ") {
			return strings.TrimSpace(line) == "exit 0"
		}
	}
	return false
}

func trim(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// THE TAIL, NOT THE HEAD, for anything long. A compiler or test runner puts
	// the summary and the first real error at the end; keeping the head keeps the
	// banner and throws away the diagnosis.
	return fmt.Sprintf("… %d characters omitted …\n%s", len(s)-max, s[len(s)-max:])
}

// stuckTemperature raises sampling as idle turns accumulate, mirroring the
// department's dev.Temperature and for its reason: a model repeating the exact
// same unproductive move at temperature 0.2 will repeat it forever, because the
// most probable continuation is the one it just tried. Heat is the mechanical
// way to break the tie when the class runs with reasoning off — measured here
// on a test author that re-read its own broken file through fifteen idle turns
// with the vet error naming the line in front of it. Progress resets idle,
// which resets this.
func stuckTemperature(base float64, idle int) float64 {
	return min(max(base, 0.15*float64(idle)), 1.0)
}

// reasoningLine flattens a reasoning trace to one log line: the head and the
// tail, because the head says what the model is worried about and the tail is
// where the decision lands, and a diagnosis usually needs both.
func reasoningLine(r string) string {
	flat := strings.Join(strings.Fields(r), " ")
	if len(flat) <= 300 {
		return flat
	}
	return flat[:180] + " … " + flat[len(flat)-100:]
}

// argHead is the head of a tool call's arguments, for the log. The HEAD, unlike
// trim, because a write's path comes first in its JSON.
func argHead(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 110 {
		s = s[:110] + "…"
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return trim(s, 120)
}
