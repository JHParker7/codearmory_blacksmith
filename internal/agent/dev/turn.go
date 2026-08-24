package dev

import (
	"context"
	"fmt"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/ticket"
)

// MaxParseFailures bounds CONSECUTIVE replies that are not valid actions.
//
// LOOSER THAN THE REFUSAL CEILING BECAUSE IT IS A DIFFERENT FAILURE. A model
// that repeats a pointless action is not going to stop on its own; a model that
// emits a stray code fence or a truncated object usually produces a clean action
// the next turn, once it is handed the parse error. Treating two bad replies as
// terminal would throw away a task over a stray fence.
const MaxParseFailures = 4

// Gateway is the model call the loop needs.
type Gateway interface {
	Chat(ctx context.Context, class model.Class, req model.ChatRequest) (model.ChatResult, error)
}

// Turn is one exchange with the model: what was asked, and what came back.
type Turn struct {
	Request model.ChatRequest
	Result  model.ChatResult
	Action  Action
	Err     error
}

// BuildRequest assembles one turn.
//
// THE MESSAGES ARE ORDERED BY VOLATILITY, not by topic, and the split is the
// whole reason RenderWorld and RenderProgress are separate functions. See
// RenderWorld: a few hundred changed characters in FRONT of the repository
// listing cost the entire prefix cache — 31.3s against 1.3s, measured.
//
// THE HISTORY GOES IN A USER TURN, between the two halves. Anything placed in
// the assistant role is a demonstration of what to produce, and replaying either
// raw completions or descriptions of them taught the model to imitate the
// replay rather than answer.
func BuildRequest(t ticket.Ticket, why Reasons, s *State, mode Mode, sys string, tools bool) model.ChatRequest {
	msgs := []model.Message{
		{Role: "system", Content: sys},
		// THE STABLE HALF FIRST, as its own message, so the prefix a backend caches
		// is the expensive one.
		{Role: "user", Content: RenderWorld(t, why, s)},
	}
	msgs = append(msgs, s.RecentHistory()...)
	msgs = append(msgs, model.Message{Role: "user", Content: RenderProgress(s)})

	req := model.ChatRequest{
		Messages:    msgs,
		Temperature: Temperature(s.Stuck()),
		MaxTokens:   model.MaxReplyTokens,
	}

	// TOOLS WHERE THE BACKEND HAS THEM, the grammar where it does not. A
	// well-behaved backend answers with a typed call whose arguments the server
	// has already validated — no envelope for the model to author, and no field
	// for it to put in the wrong place.
	if tools {
		req.Tools = Tools(mode)
	} else {
		req.Schema = Schema()
	}
	return req
}

// ReadReply turns a completion into an action, and records what the model said
// around it.
//
// THE PROSE IS KEPT AND THE SYNTAX IS DROPPED, and the split matters both ways.
// Replaying raw completions fed the model bare JSON and it began imitating the
// shape rather than choosing a tool, fabricating {"error":{...}} and
// {"tool_call_id":...} envelopes within minutes. But stripping the syntax first
// took the prose with it, which threw away the thing worth keeping: "I wrote
// main.go lines 31-41" does not tell the next turn that it had already concluded
// there were duplicate NewStore declarations and already tried removing them.
// That conclusion is what it kept re-deriving, four commits running.
//
// Prose is affordable where the state is not. The slot holds 16,384 tokens and
// the rendered state is 4-6k of it; twenty turns of reasoning is 2-4k, while
// twenty replayed states would exhaust the slot in three.
func ReadReply(res model.ChatResult, mode Mode) (Action, error) {
	return ParseReply(res, mode)
}

// TurnRecord is the one-line summary of an action for the history.
func TurnRecord(act Action, reply string) string {
	record := act.Action + ": " + StepDetail(act)
	if why := ProseOf(reply); why != "" {
		record += "\n" + why
	}
	return record
}

// ParseFailureNotice tells the model why its reply was not usable.
//
// TRUNCATION AND MALFORMEDNESS READ THE SAME AND NEED OPPOSITE FIXES. "Your
// reply was rejected" makes a model send the same thing again; "you were cut
// off" makes it write less. The finish reason is the only thing that separates
// them, so it is reported rather than guessed at.
func ParseFailureNotice(res model.ChatResult, err error) string {
	if res.Truncated(model.MaxReplyTokens) {
		return "Your previous reply was CUT OFF at the token limit — it was not malformed, it was " +
			"too long. Do not send it again unchanged. Make a SMALLER edit: write one function or " +
			"one section per turn, and use a search/replace edit against text already in the file " +
			"rather than sending a whole file at once."
	}
	return "Your previous reply was rejected: " + err.Error()
}

// Budget is the turn allowance including the refunds.
//
// THE REFUNDS EXTEND THE BUDGET rather than being deducted from it, and the
// prompt has to show the same number the loop enforces — otherwise the agent
// counts down to zero and keeps going, which is the one thing worse than a wrong
// number.
func Budget(maxIterations, refunded int) int { return maxIterations + refunded }

// Advance records one action against the state and reports whether the tree
// changed enough to be worth verifying.
//
// ONE PLACE, so a counter added later is maintained on every path rather than on
// the paths someone remembered. Every counter here has, at some point, been
// updated in one branch and forgotten in another.
func (s *State) Advance(act Action, mode Mode) (verify bool, err error) {
	s.Trail = append(s.Trail, Step{Action: act.Action, Detail: StepDetail(act)})

	if act.Summary != "" {
		s.Summary = act.Summary
	}
	if act.Type != "" {
		s.CommitType = act.Type
	}

	switch act.Action {
	case ActionReadFiles:
		s.ConsecutiveReads++
		// The refund is applied here rather than at the call site so every read
		// path gets it. See RefundReads.
		if RefundReads {
			s.Refunded++
		}
		return false, nil

	case ActionUndoEdit:
		s.ConsecutiveReads = 0
		what, ok := s.Undo()
		if !ok {
			s.NoProgress("Rejected: there is no edit of yours to undo — nothing has been " +
				"written this attempt.")
			return false, nil
		}
		// A REWIND IS PROGRESS, not a refusal: it costs a turn and buys a file the
		// agent can address again, which is strictly better than editing blind
		// against a file it has broken.
		s.Progress()
		s.Notice = "Undone. Restored: " + what + ". The file is back to what it was before " +
			"your last write; read it if you are unsure what it now contains."
		return false, nil

	case ActionWriteFiles:
		s.ConsecutiveReads = 0
		before := TreeHash(s.Staged)

		// LET THE TOOL RUN. This once refused a write when too many had piled up
		// unverified, on the reasoning that writing again could not tell the model
		// whether any of it worked. That reasoning was sound and the effect was
		// not: a refusal teaches nothing a result would not teach better, and
		// blocking one repeated action only moves the repetition to another —
		// measured directly, an agent barred from re-reading spent 49 of 77 turns
		// on refused verifications instead.
		if err := Apply(s, act.Edits, mode); err != nil {
			s.NoProgress("Rejected: " + err.Error())
			return false, err
		}

		// A WRITE THAT CHANGED NOTHING IS NOT A WRITE. The tree is compared rather
		// than a counter incremented, because nine separate code paths lost track
		// of a counter in one day of runs.
		if TreeHash(s.Staged) == before {
			s.NoopEdits++
			s.NoProgress(NoopEditNotice(s.NoopEdits, s.TreeVerified(), s.TestsPass, s.LastTest))
			return false, nil
		}

		s.NoopEdits = 0
		s.Progress()
		// THE CHECKS FOLLOW A REAL EDIT BY THEMSELVES. The model cannot ask for
		// them, so an edit that changed the tree is the whole trigger.
		return true, nil
	}

	// Unreachable through ParseReply, which refuses an unknown action. Handled
	// rather than ignored so a vocabulary added later cannot silently do nothing.
	s.NoProgress(fmt.Sprintf("Rejected: %q is not an action this stage can take.", act.Action))
	return false, nil
}

// RecordVerification files what the checks decided, and resets what that
// decision has made stale.
func (s *State) RecordVerification(output string, pass bool) {
	s.LastTest = output
	s.TestsPass = pass
	// THE FINGERPRINT, NOT A COUNTER. See TreeHash.
	s.VerifiedTree = TreeHash(s.Staged)
	if !pass {
		s.FailedVerifications++
		return
	}
	s.FailedVerifications = 0
}

// Finished reports whether the stage's own success condition is met, so the loop
// can end without the model having to say so.
//
// THE STAGE ENDS BY ITSELF. Across 3,498 recorded turns the agents called finish
// once, and one ran to iteration 439 still reading — so waiting to be told is
// waiting for something that does not come.
func (s *State) Finished() bool {
	return s.TestsPass && s.TreeVerified() && len(s.Staged) > 0
}

// StopParsing reports whether a run of unusable replies should end the attempt,
// and what to say about it.
func (s *State) StopParsing() (string, bool) {
	if s.ParseFails < MaxParseFailures {
		return "", false
	}
	return fmt.Sprintf("%d replies in a row could not be parsed as an action", s.ParseFails), true
}

// TrailSummary renders what the attempt actually did, so a failure that produced
// no test output still says something a person can act on.
//
// AN EXHAUSTED ATTEMPT OTHERWISE REPORTS ONLY ITS LAST TEST OUTPUT, and one that
// ended on a write has none at all — so the ticket says "stopped after 8
// iterations" and nothing else, which is unusable.
func TrailSummary(trail []Step) string {
	if len(trail) == 0 {
		return "it took no actions at all"
	}
	counts := map[string]int{}
	var order []string
	for _, s := range trail {
		if counts[s.Action] == 0 {
			order = append(order, s.Action)
		}
		counts[s.Action]++
	}
	parts := make([]string, 0, len(order))
	for _, name := range order {
		parts = append(parts, fmt.Sprintf("%d× %s", counts[name], name))
	}
	return fmt.Sprintf("%d actions (%s); the last was %s",
		len(trail), strings.Join(parts, ", "), trail[len(trail)-1])
}
