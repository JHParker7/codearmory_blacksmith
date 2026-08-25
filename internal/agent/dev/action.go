// Package dev is the developer: the one agent that changes code.
//
// This file is its VOCABULARY — what a turn may say, how that reaches the model,
// and how a reply is read back. It is separated from the loop because almost
// every expensive failure in this repo's history was a vocabulary failure rather
// than a reasoning one: the model understood the task and could not express the
// change, or expressed it in a shape the harness did not accept.
//
// TWO PRINCIPLES RUN THROUGH ALL OF IT.
//
// Make a mistake unrepresentable rather than forbidden. Repeatedly, a message
// telling the model not to do something did not stop it and a schema did. The
// edit shapes below branch by HOW an edit says where it goes, so a quote cannot
// sit beside its own replacement and be copied into it.
//
// Accept what the model actually emits. A reply that did what was asked must not
// be refused for a reason the model has no way to see — so a tool call written
// into the content is still a tool call, and a hand-written envelope is read too.
package dev

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/edit"
	"github.com/code-armory-app/blacksmith/internal/model"
)

// Action is the one thing a model turn may request.
type Action struct {
	Action string   `json:"action"`
	Paths  []string `json:"paths,omitempty"`

	// Edits are addressed changes.
	//
	// WHOLE-FILE WRITES WERE THE PREVIOUS SHAPE AND ARE GONE: replacing a file
	// wholesale makes every write a chance to silently drop the parts the ticket
	// never mentioned, which is exactly what happened — a ticket asking for one
	// struct deleted an entire HTTP server, and only the reviewer noticed. An
	// edit that names what it is replacing cannot do that, and says loudly when
	// the file is not what the model thought.
	Edits []edit.Edit `json:"edits,omitempty"`

	Summary string `json:"summary,omitempty"`

	// Type is the Conventional Commits type. Asked of the model because only it
	// knows what the change was, and validated against an allowlist because a
	// commit-msg hook will reject anything else — so a message outside it is not
	// a style preference, it is a commit that will not land.
	Type string `json:"type,omitempty"`

	Reason string `json:"reason,omitempty"`
}

// ConventionalTypes is the Conventional Commits allowlist, as commitlint
// enforces it on the commit-msg hook.
var ConventionalTypes = []string{
	"feat", "fix", "chore", "docs", "refactor", "test", "perf", "build", "ci", "style",
}

// The action names. Anything else is refused and fed back as an error, which is
// more useful to the model than a silent no-op.
const (
	ActionReadFiles  = "read_files"
	ActionWriteFiles = "write_files"

	// ActionUndoEdit restores a file to what it was before the last edit.
	//
	// THE MISSING ESCAPE HATCH. Once a file diverges from the model's mental
	// image of it, every addressing scheme fails together: text no longer matches
	// and line numbers no longer mean what it thinks. Recorded here before it had
	// a name — a developer wrote store.go with every struct tag missing its
	// closing backtick, then spent 34 consecutive actions failing to search and
	// replace its way out of a file it had itself broken.
	//
	// Both edit formats have now failed for this same reason, which is why the
	// recovery matters more than the choice between them.
	ActionUndoEdit = "undo_edit"

	// The three below are DEFINED BUT NOT OFFERED. See Actions.
	ActionRunTests = "run_tests"
	ActionFinish   = "finish"
	ActionGiveUp   = "give_up"
)

// Actions is the vocabulary a stage may use.
//
// THE MODEL CHOOSES WHAT TO CHANGE, NOT WHEN TO CHECK IT.
//
// run_tests, finish and give_up were removed together, and the measurement is
// blunt: in the last fixture pass 28 of 31 refusals were the harness telling the
// agent it could not run the tests just now. Every one of those existed only
// because asking was possible. Verification is not a decision the model is good
// at sequencing — it is a consequence of having changed something, so the
// harness runs it on every write that changes the tree and ends the stage the
// moment its own success condition is met.
//
// give_up went with them because it was never the real bound. An agent that
// cannot finish is already ended by the refusal ceiling, the read ceiling and
// the turn budget; a tool for quitting only gave a stuck model a way to convert
// a recoverable attempt into a terminal one.
//
// EVERY STAGE IS ENDED BY ITS GATE, INCLUDING THE SPECIFICATION AUTHOR. Giving
// the author finish back was tried, on the reasoning that a passing check means
// something different for it — its tests are supposed to fail, so "the gate
// passed" is as true after the first file as after the last, and on r89 that
// truncated an author briefed on five units of work to one file in three turns.
//
// The truncation was real and the fix was wrong. Measured on r90: with finish
// restored the same author ran 49 turns and wrote the same file 41 times, never
// finishing. That is what the tool was removed for — across 3,498 recorded turns
// the agents called finish once, and one ran to iteration 439 still reading.
//
// The truncation is answered by the BRIEF instead: an author given ONE unit of
// work is finished when its first file compiles, so the gate and the job end at
// the same moment.
var Actions = []string{ActionReadFiles, ActionWriteFiles, ActionUndoEdit}

// Bounds on what one iteration may move.
//
// A model that asks for the whole repository blows the context window and
// produces worse output than one that asks for four files, so these are quality
// controls as much as safety ones.
const (
	MaxReadPaths  = 12
	MaxWriteFiles = 12

	// MaxSummaryRunes bounds the one-line summary. It is also a GRAMMAR bound —
	// see Schema — so it is not merely cosmetic.
	MaxSummaryRunes = 200

	// MaxAnchorChars bounds a quoted anchor. Long enough to disambiguate anything
	// that needs it and short enough that quoting a whole function is no longer
	// expressible, which is what made the quote a copy of the replacement.
	MaxAnchorChars = 400

	MaxPathChars = 200
	MaxDeclChars = 120
)

// Mode is which stage is driving the loop. The vocabulary is shared; what
// differs is which files it may touch and what its check means.
type Mode int

const (
	ModeDevelop Mode = iota
	ModeTest
	ModeCoverage
)

// EditRule is the sentence stating which files this stage may write.
//
// IT LIVES ON THE TOOL, not only in the system prompt. A rule stated where the
// model is choosing arguments is followed more often than the same rule stated
// two thousand tokens earlier.
func (m Mode) EditRule() string {
	switch m {
	case ModeTest:
		return "Write the tests. You may ONLY edit *_test.go files."
	case ModeCoverage:
		return "Add tests. You may ONLY create NEW *_test.go files — the tests that were " +
			"here before you are the specification and cannot be edited."
	default:
		return "Edit the implementation. You may NOT edit *_test.go files."
	}
}

// CheckDescription says what this stage's check actually does, which differs by
// stage and is the thing a model most often assumes wrongly.
func (m Mode) CheckDescription() string {
	switch m {
	case ModeCoverage:
		return "Run the suite and report statement coverage. The suite must pass and coverage " +
			"must reach the target before you finish."
	case ModeTest:
		return "Push your tests and check they are VALID GO. It does not run them — they cannot " +
			"pass yet, because the code they describe does not exist."
	default:
		return "Push the branch and run the repository's own pipeline against it. Must pass " +
			"before you finish."
	}
}

// ParseReply reads one model turn.
//
// THREE CHANNELS, ALL ACCEPTED, none preferred by taste: a parsed tool call, a
// tool call the model wrote into its content, and a bare JSON envelope. Refusing
// any of them would mean rejecting a model that did exactly what it was asked,
// for a reason it has no way to see.
func ParseReply(res model.ChatResult, mode Mode) (Action, error) {
	if len(res.Calls) > 0 {
		return ActionFromCall(res.Calls[0], mode)
	}
	// A TOOL CALL EMITTED AS TEXT IS STILL A TOOL CALL. Not every model's chat
	// template is parsed into tool_calls by the server — qwen2.5-coder writes
	// {"name":…,"arguments":{…}} inside <tools> tags and llama.cpp hands it back
	// as content.
	if call, ok := ToolCallFromText(res.Content); ok {
		return ActionFromCall(call, mode)
	}
	return ParseAction(res.Content)
}

// ActionFromCall maps a tool call onto the loop's action.
func ActionFromCall(call model.ToolCall, mode Mode) (Action, error) {
	name := strings.TrimSpace(call.Name)
	allowed := ActionsFor(mode)
	if !slices.Contains(allowed, name) {
		return Action{}, fmt.Errorf("unknown tool %q; must be one of %s", name, strings.Join(allowed, ", "))
	}

	act := Action{Action: name}
	args := strings.TrimSpace(call.Arguments)
	if args == "" || args == "null" {
		return act, nil
	}
	if err := model.DecodeJSON(args, &act); err != nil {
		return Action{}, fmt.Errorf("tool %s: arguments are not valid JSON: %w", name, err)
	}
	// ARGUMENTS MUST NEVER REDEFINE WHICH TOOL WAS CALLED. The name is the one
	// field the caller already knows, and a model that repeats it wrongly would
	// otherwise run a different action than the one it selected.
	act.Action = name
	return act, nil
}

// ParseAction is the content fallback: a bare JSON object naming the action.
func ParseAction(raw string) (Action, error) {
	var act Action
	// DecodeObject leaves a reply that already IS an object unfenced, which
	// matters here: this path carries file contents — a markdown file, a doc
	// comment with an example in it — so a fence inside the payload is ordinary,
	// and unwrapping one would return the payload's snippet and throw the action
	// away.
	if err := model.DecodeObject(raw, &act); err != nil {
		return Action{}, fmt.Errorf("reply is not valid JSON: %w", err)
	}

	act.Action = strings.TrimSpace(act.Action)
	if act.Action == "" {
		return Action{}, errors.New(`reply has no "action" field`)
	}
	// finish is accepted here though it is not offered: a model that asks to
	// finish has said something meaningful, and the loop decides what to do with
	// it. An unknown word has not.
	if !slices.Contains(Actions, act.Action) && act.Action != ActionFinish {
		return Action{}, fmt.Errorf("unknown action %q; must be one of %s",
			act.Action, strings.Join(Actions, ", "))
	}
	return act, nil
}

// ActionsFor is the vocabulary one stage may use. Every stage has the same one
// today; the parameter is kept because the stages differ in everything else and
// a caller should not have to know that this is the exception.
func ActionsFor(Mode) []string { return Actions }

// ToolCallWrappers are the tags models wrap a written-out tool call in.
var ToolCallWrappers = []string{"tools", "tool_call", "tool_calls", "function_call"}

// ToolCallFromText recovers a tool call a model wrote into its content.
//
// The shape is the one the tool-calling API itself uses — a name and an
// arguments object — which is what distinguishes it from the hand-written
// envelope ParseAction reads.
func ToolCallFromText(content string) (model.ToolCall, bool) {
	s := strings.TrimSpace(content)
	if s == "" {
		return model.ToolCall{}, false
	}
	for _, tag := range ToolCallWrappers {
		if i := strings.Index(s, "<"+tag+">"); i >= 0 {
			rest := s[i+len(tag)+2:]
			if j := strings.Index(rest, "</"+tag+">"); j >= 0 {
				rest = rest[:j]
			}
			s = strings.TrimSpace(rest)
			break
		}
	}
	if fenced := model.Unfence(s); fenced != "" {
		s = fenced
	}
	start, end := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if start < 0 || end < start {
		return model.ToolCall{}, false
	}

	var raw struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := model.DecodeJSON(s[start:end+1], &raw); err != nil {
		return model.ToolCall{}, false
	}
	if raw.Name == "" {
		// An envelope, not a tool call: let ParseAction read it instead.
		return model.ToolCall{}, false
	}
	args := strings.TrimSpace(string(raw.Arguments))
	if args == "" || args == "null" {
		args = "{}"
	}
	return model.ToolCall{Name: raw.Name, Arguments: args}, true
}
