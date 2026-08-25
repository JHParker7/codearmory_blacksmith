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

	"github.com/code-armory-app/blacksmith/internal/record"
)

// Action is the one thing a model turn may request.
type Action struct {
	Action string     `json:"action"`
	Paths  StringList `json:"paths,omitempty"`

	// Edits are addressed changes.
	//
	// WHOLE-FILE WRITES WERE THE PREVIOUS SHAPE AND ARE GONE: replacing a file
	// wholesale makes every write a chance to silently drop the parts the ticket
	// never mentioned, which is exactly what happened — a ticket asking for one
	// struct deleted an entire HTTP server, and only the reviewer noticed. An
	// edit that names what it is replacing cannot do that, and says loudly when
	// the file is not what the model thought.
	Edits EditList `json:"edits,omitempty"`

	Summary string `json:"summary,omitempty"`

	// Type is the Conventional Commits type. Asked of the model because only it
	// knows what the change was, and validated against an allowlist because a
	// commit-msg hook will reject anything else — so a message outside it is not
	// a style preference, it is a commit that will not land.
	Type string `json:"type,omitempty"`

	Reason string `json:"reason,omitempty"`

	// The flat write's edit, carried at the top level rather than inside an
	// array. Only ActionWriteFile populates these; flatEdit folds them into
	// Edits so nothing downstream has to know which wire shape arrived.
	Path      string `json:"path,omitempty"`
	OldStr    string `json:"old_str,omitempty"`
	Decl      string `json:"decl,omitempty"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
	Replace   string `json:"replace,omitempty"`
}

// flatEdit turns a flat write call into the one edit it describes.
//
// THE SCHEMA NO LONGER SAYS "EXACTLY ONE ADDRESS", so this does. The array form
// expressed the three addressing modes as a oneOf, which made an edit naming two
// of them unrepresentable — and unrepresentable is the whole reason the nesting
// was there. Flattening bought a shape the model can actually close, at the cost
// of having to refuse the ambiguity in words. The refusal names the modes it
// found, because "invalid edit" sends a correct agent to the wrong place.
func flatEdit(act Action) (edit.Edit, error) {
	e := edit.Edit{
		Path:      strings.TrimSpace(act.Path),
		OldStr:    act.OldStr,
		Decl:      strings.TrimSpace(act.Decl),
		StartLine: act.StartLine,
		EndLine:   act.EndLine,
		Replace:   act.Replace,
	}
	if e.Path == "" {
		return edit.Edit{}, errors.New(`tool write_file: "path" is required — name the file to edit`)
	}
	if _, ok := e.Address(); !ok {
		return edit.Edit{}, fmt.Errorf(`tool write_file: an edit says WHERE in exactly one `+
			`way, and this gave %s. Use "old_str" to quote what you are replacing, OR `+
			`"decl" to name a whole declaration, OR "start_line"/"end_line", OR none of `+
			`them to write the whole file`, gaveWhich(e))
	}
	return e, nil
}

// gaveWhich names the addressing fields that were actually set, so the refusal
// can say what to remove rather than only what the rule is.
func gaveWhich(e edit.Edit) string {
	var got []string
	if e.OldStr != "" {
		got = append(got, `"old_str"`)
	}
	if e.Decl != "" {
		got = append(got, `"decl"`)
	}
	if e.StartLine > 0 || e.EndLine > 0 {
		got = append(got, `"start_line"/"end_line"`)
	}
	if len(got) == 0 {
		return "none of them with a line range that is not whole-file"
	}
	return strings.Join(got, " and ")
}

// EditList is the edits, and it accepts the array EITHER AS AN ARRAY OR AS A
// JSON STRING CONTAINING ONE.
//
// THE MODEL DOUBLE-ENCODES ITS OWN ARGUMENTS, and it is not a rare slip. Every
// hosted tool-calling API takes the arguments object as a string, so a model
// that has learned to stringify its arguments stringifies the values inside them
// too, and emits:
//
//	{"tool":"write_files","arguments":{"edits":"[{\"path\": \"store_test.go\", ...}]"}}
//
// Refusing that is refusing a reply that DID WHAT WAS ASKED for a reason the
// model cannot see: the content is right, the assertions are right, only the
// quoting is one layer deep. Measured on a live run — a spec-agent emitted this
// exact shape on three consecutive attempts, ~3,500 completion tokens each, was
// told only "unparseable model output", and the stage died having thrown away
// work that was correct.
//
// Accepting it here is the file's own principle: make the mistake
// unrepresentable, and where it cannot be, accept what the model actually emits.
type EditList []edit.Edit

func (l *EditList) UnmarshalJSON(b []byte) error {
	var direct []edit.Edit
	if err := json.Unmarshal(b, &direct); err == nil {
		*l = direct
		return nil
	}

	var wrapped string
	if err := json.Unmarshal(b, &wrapped); err != nil {
		// Neither an array nor a string. Report the ARRAY's error, because that is
		// the shape that was asked for and the one worth correcting.
		return json.Unmarshal(b, &direct)
	}
	if err := json.Unmarshal([]byte(wrapped), &direct); err != nil {
		return fmt.Errorf("edits arrived as a string, and its contents are not a "+
			"JSON array of edits: %w", err)
	}
	*l = direct
	return nil
}

// StringList is the same tolerance for a list of paths, which arrives
// double-encoded from the same models for the same reason.
type StringList []string

func (l *StringList) UnmarshalJSON(b []byte) error {
	var direct []string
	if err := json.Unmarshal(b, &direct); err == nil {
		*l = direct
		return nil
	}

	var wrapped string
	if err := json.Unmarshal(b, &wrapped); err != nil {
		return json.Unmarshal(b, &direct)
	}
	// A BARE PATH IS NOT AN ERROR EITHER. "main.go" is a string that is not a
	// JSON array, and it plainly means one file.
	if err := json.Unmarshal([]byte(wrapped), &direct); err != nil {
		trimmed := strings.TrimSpace(wrapped)
		if trimmed == "" || strings.HasPrefix(trimmed, "[") {
			return fmt.Errorf("paths arrived as a string, and its contents are not "+
				"a JSON array of paths: %w", err)
		}
		*l = StringList{trimmed}
		return nil
	}
	*l = direct
	return nil
}

// ConventionalTypes is the Conventional Commits allowlist, as commitlint
// enforces it on the commit-msg hook.
var ConventionalTypes = []string{
	"feat", "fix", "chore", "docs", "refactor", "test", "perf", "build", "ci", "style",
}

// The action names. Anything else is refused and fed back as an error, which is
// more useful to the model than a silent no-op.
const (
	ActionReadFiles = "read_files"

	// ActionWriteFile is ONE edit per call, with its address and its text as
	// TOP-LEVEL arguments. See Tools for why the nesting had to go.
	ActionWriteFile = "write_file"

	// ActionWriteFiles is the array form. STILL ACCEPTED, NEVER OFFERED: a
	// backend whose tool calls really are schema-constrained emits it correctly,
	// and refusing a reply that did what was asked is the failure this package
	// exists to avoid. It is simply not what the model is asked for.
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
var Actions = []string{ActionReadFiles, ActionWriteFile, ActionWriteFiles, ActionUndoEdit}

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

	// ModeSpecMerge reconciles the sections several authors wrote onto ONE
	// branch so they compile as one package.
	//
	// It exists because the sections are written by agents that cannot see each
	// other: two of them declare the same helper, or the same fixture type, and
	// the package then does not build — which the developer would be handed as
	// though it were its own fault. It may edit only test files, because the
	// implementation is not its to change, and it MAY WEAKEN NOTHING: every
	// assertion that was there must still be there when it finishes.
	ModeSpecMerge
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
	case ModeSpecMerge:
		return "Reconcile the tests on this branch so they compile as ONE package. You may " +
			"ONLY edit *_test.go files, and you may WEAKEN NOTHING: rename a duplicate, fold " +
			"two identical helpers into one, and leave every assertion standing."
	default:
		return "Edit the implementation. You may NOT edit *_test.go files."
	}
}

// BranchMarker is the marker this stage announces its push with.
//
// A MARKER, NOT THE ROLE NAME. The reviewer, the integrator and the resolver all
// decide whether a ticket has anything to act on by looking for one of the three
// markers, so a comment headed "dev-agent" publishes a branch that nothing can
// find. Measured on a clean run: the developer finished in 23 turns, wrote its
// branch, and the ticket sat in ready_for_review for half an hour because
// HasBranch answered no. The pipeline dead-ends there — every stage before it
// succeeds and nothing after it ever starts.
//
// Which marker matters as well as that there is one: they name what was pushed,
// and a route through the pipeline that skipped the developer still has to be
// readable by whoever reads it next.
func (m Mode) BranchMarker() string {
	switch m {
	case ModeTest, ModeSpecMerge:
		return record.TestsWrittenMarker
	case ModeCoverage:
		return record.CoverageMarker
	default:
		return record.BranchMarker
	}
}

// WritesSpec reports whether this stage is the one AUTHORING the tests, and so
// owns whether they compile. Both such stages may edit test files and only test
// files; every other stage is handed the result and may not touch it.
func (m Mode) WritesSpec() bool {
	return m == ModeTest || m == ModeSpecMerge
}

// CheckDescription says what this stage's check actually does, which differs by
// stage and is the thing a model most often assumes wrongly.
func (m Mode) CheckDescription() string {
	switch m {
	case ModeCoverage:
		return "Run the suite and report statement coverage. The suite must pass and coverage " +
			"must reach the target before you finish."
	case ModeTest, ModeSpecMerge:
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

	// THE FLAT CALL CARRIES ITS EDIT AT THE TOP LEVEL, so it is lifted into the
	// same one-element list every other path produces. Everything downstream sees
	// one shape; only the wire differs.
	if name == ActionWriteFile {
		one, err := flatEdit(act)
		if err != nil {
			return Action{}, err
		}
		act.Edits = EditList{one}
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

	// The flat form arrives here too, when the model answers in content rather
	// than through a tool. Lifted the same way, so both wires produce one shape.
	if act.Action == ActionWriteFile {
		one, err := flatEdit(act)
		if err != nil {
			return Action{}, err
		}
		act.Edits = EditList{one}
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
