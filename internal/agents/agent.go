// Package agents is the model loop and the roles that run in it.
//
// It holds the ONLY calls to the model in the rebuilt pipeline. Everything a
// stage does that is not "ask the model something" is a tool call, and the tools
// live in internal/tools; what remains here is a loop, a budget, and one
// description per role.
//
// The roles are data rather than code — see roles.go — because the differences
// between an architect and a developer that actually matter are a prompt, a set
// of tools and a write guard. Every time one of those differences was expressed
// as a separate code path instead, the paths drifted.
package agents

import (
	"context"
	"fmt"
	"strings"

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
	// what the check said.
	LastCheck string

	Trail []Step
}

// Agent runs one role to completion.
type Agent struct {
	Role    Role
	Tools   *tools.Set
	Gateway Gateway

	// Log receives one line per turn. Nil is fine. Present because a stage that
	// takes twenty minutes with no output is indistinguishable from a hung one.
	Log func(string)
}

// Run works the task until the role's check passes or its budget runs out.
//
// THE PROMPT IS REBUILT EVERY TURN rather than accumulated as a conversation.
// That is this department's convention and it is load-bearing twice over: the
// prompt stays bounded no matter how long the stage runs, and a thinking model's
// scratchpad never gets replayed to it as though it were part of the dialogue,
// which is how one talks itself into a loop. What carries between turns is the
// trail — what was tried and what came back — rendered fresh each time.
func (a *Agent) Run(ctx context.Context, task string) (Outcome, error) {
	out := Outcome{}

	for i := 0; i < a.Role.MaxIterations; i++ {
		out.Iterations = i + 1

		res, err := a.Gateway.Chat(ctx, a.Role.Class, model.ChatRequest{
			Messages:    a.messages(task, out.Trail, out.LastCheck),
			Temperature: a.Role.Temperature,
			MaxTokens:   a.Role.MaxTokens,
			Tools:       a.Tools.Definitions(),
		})
		if err != nil {
			return out, fmt.Errorf("%s: turn %d: %w", a.Role.Name, out.Iterations, err)
		}

		// NO TOOL CALL MEANS IT ANSWERED. For a stage with a check that is
		// premature — the check is what decides — so it gets told so and the turn
		// is spent. For a stage without one, the answer IS the deliverable.
		if len(res.Calls) == 0 {
			if a.Role.Check == "" {
				out.Answer = res.Content
				out.Passed = true
				a.logf("%s: finished after %d turns", a.Role.Name, out.Iterations)
				return out, nil
			}
			out.Trail = append(out.Trail, Step{
				Tool: "(no tool call)",
				Args: trim(res.Content, 400),
				Result: "You answered in prose, but this stage ends when its check passes, not " +
					"when you say it is done. Call " + tools.RunCommand + " to run the check, or " +
					tools.WriteFile + " to change something first.",
			})
			continue
		}

		for _, call := range res.Calls {
			result, err := a.Tools.Invoke(ctx, call.Name, call.Arguments)
			if err != nil {
				// The sandbox is unreachable or similar. Not something the model can
				// reason its way out of, so it ends the stage rather than becoming a
				// refusal it would keep retrying.
				return out, fmt.Errorf("%s: %s: %w", a.Role.Name, call.Name, err)
			}
			a.logf("%s: %s -> %s", a.Role.Name, call.Name, firstLine(result))
			out.Trail = append(out.Trail, Step{
				Tool:   call.Name,
				Args:   trim(call.Arguments, 300),
				Result: result,
			})

			if call.Name == tools.RunCommand {
				out.LastCheck = result
				if checkPassed(result) {
					out.Passed = true
					a.logf("%s: check passed after %d turns", a.Role.Name, out.Iterations)
					return out, nil
				}
			}
		}
	}

	a.logf("%s: out of budget after %d turns", a.Role.Name, out.Iterations)
	return out, nil
}

func (a *Agent) logf(format string, args ...any) {
	if a.Log != nil {
		a.Log(fmt.Sprintf(format, args...))
	}
}

// messages renders the whole prompt for one turn.
func (a *Agent) messages(task string, trail []Step, lastCheck string) []model.Message {
	var b strings.Builder
	b.WriteString(task)

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
		{Role: "system", Content: a.Role.System},
		{Role: "user", Content: b.String()},
	}
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
		fmt.Fprintf(&b, "%d. %s %s\n   -> %s\n", from+i+1, s.Tool, s.Args, trim(s.Result, 1200))
	}
	return b.String()
}

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

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return trim(s, 120)
}
