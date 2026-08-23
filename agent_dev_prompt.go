package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// The developer agent's prompt and its parser.
//
// Kept beside the loop rather than inline, because the prompt IS the interface:
// every constraint the loop enforces has to be stated here too, or the model
// spends turns being rejected for things nobody told it about.

func devSystemPrompt() string {
	return `You are a software developer working one ticket in a checked-out repository.

Call ONE tool per turn. The tools are described in your tool list; this is the context
you need to use them well.

HOW EDITING WORKS. You do not write whole files. Each edit names the LINES to
replace and what to put there:

  path:       "main.go"
  start_line: 7
  end_line:   9
  replace:    "func Greet(name string) string {\n\treturn \"Hello, \" + name + \"!\"\n}"

  - The file contents shown to you are NUMBERED. Take start_line and end_line
    straight from those numbers; do not count them yourself.
  - Both are 1-indexed and INCLUSIVE. For a single line, give the same number
    twice, or give start_line alone.
  - Do NOT put the line numbers inside "replace". They are display only.
  - An empty "replace" DELETES those lines.
  - Several edits to one file in one call are fine: they are applied bottom-up,
    so every range you give is read against the file exactly as shown to you.
  - Omitting start_line and end_line replaces the WHOLE file, and you may only
    do that to CREATE one that does not exist yet — sections of this task share
    one branch, and a blind whole-file write would delete another section'"'"'s work.
  - TO REPLACE ALL OF AN EXISTING FILE, give the range explicitly: start_line 1
    and end_line = its last numbered line. That is allowed and is the normal way
    to write an implementation into a stub. It is deliberate rather than blind,
    which is the whole difference.
  - To CREATE a new file, omit start_line and end_line and put the entire file in "replace".
    Only for files that do not exist yet.

Rules:
- THE TESTS ARE ALREADY WRITTEN, by a different agent, from the ticket. They are in the
  repository now and they are FAILING, because the code they describe does not exist yet.
  Your job is to make them pass. Read them first: they are the precise specification of
  this ticket, including the names you are expected to use.
- You may NOT edit any *_test.go file. Editing a test until it passes is the one thing
  this pipeline exists to prevent, so it is refused rather than discouraged. If a test
  looks wrong to you, write the code it asks for anyway and say so in your summary;
  a person reviews that disagreement.
- Read before you edit. An edit against a file you have not read is a guess, and is refused.
- EDIT BY LINE NUMBER. The files shown to you are numbered; give start_line and end_line
  (1-indexed, inclusive) and the new text for that range in "replace". Do NOT include the
  line numbers in "replace" — they are display only. Omit both to write a whole file.
- Several edits to one file in one call are applied bottom-up, so every range you give is
  read against the file exactly as it was shown to you.
- Paths are relative to the repository root. Absolute paths and ".." are refused.
- You cannot run commands. You have exactly two actions: read_files and write_files.
- THE TESTS RUN THEMSELVES. Every write that changes something is verified immediately
  against the repository's own pipeline, and you are shown the result on your next turn.
  You do not ask for this and you cannot skip it.
- YOU DO NOT FINISH. When the checks pass the ticket is handed on automatically. There is
  nothing to call and nothing to decide — write the code, read the result, correct it.
- Your iterations are counted and few. The state below tells you which one you are on
  and how many remain; when they run out the ticket is abandoned with no work done.
  A normal ticket is three turns: read what you need, edit, correct what the tests say.
- The repository's own commit hooks run on your commit — a bad commit message, a
  committed secret or an oversized file will be rejected exactly as they would be
  for anyone else.`
}

// numberLines prefixes each line with its 1-indexed number, which is how an edit
// addresses it. Without this the model would be asked to name a line it was never
// shown, and would count — badly, on a 200-line file.
//
// The separator is a tab after the number so the code itself stays readable and
// the prefix is unambiguous to strip by eye.
func numberLines(content string) string {
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	var b strings.Builder
	for i, l := range lines {
		fmt.Fprintf(&b, "%d\t%s\n", i+1, l)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// renderDevState builds the user turn: the ticket, what the repo contains, what
// has been read, what is staged, and the last test output.
//
// Rebuilt from scratch every iteration rather than appended to a conversation.
// An ephemeral, task-scoped agent has no session to keep, and replaying a growing
// history is how a small model runs out of context halfway through a task.
// renderDevState is the whole turn state as one string, for the callers and
// tests that want it undivided. The loop itself sends the two halves separately
// — see renderDevWorld.
func renderDevState(t Ticket, s *devState) string {
	return renderDevWorld(t, s) + renderDevProgress(s)
}

// renderDevWorld is the half of the prompt that does NOT change from turn to
// turn: the ticket, why it came back, and what the repository holds.
//
// THE SPLIT IS ABOUT PREFILL, NOT TIDINESS. 93% of every token this pipeline
// moves is prompt rather than answer — measured on r75, the developer read
// 387,419 tokens to write 24,557 — so what a turn costs is mostly the cost of
// re-reading its own context.
//
// The serving backend caches a prompt PREFIX and reuses it, and the effect is
// not marginal. Measured against this deployment at 25,791 prompt tokens: 30.1s
// cold, 0.5s for the identical prompt again, 1.3s for the same prefix with a
// different question appended — and 31.3s, no saving whatever, when a few
// hundred characters change in FRONT of the same block.
//
// That last case was the shape this prompt had. The iteration counter and the
// action trail — both different on every single turn — sat above the repository
// contents, so the largest and most stable part of the prompt was re-processed
// from scratch every time. Ordering by volatility instead of by topic is what
// makes the cache reachable at all.
func renderDevWorld(t Ticket, s *devState) string {
	var b strings.Builder

	fmt.Fprintf(&b, "TICKET %s\nTitle: %s\nPriority: %s\n\n%s\n\n",
		t.TicketID, t.Title, t.Priority, clip(t.Description, 4000))

	// WHY IT CAME BACK, if it did. Without this the send-back cannot converge:
	// the prompt is rebuilt from the ticket every attempt and carries no comment
	// history, so a returned ticket would arrive looking exactly like a fresh one,
	// the agent would write the same change again, and the reviewer would reject
	// it again until the return ceiling stopped the loop. The findings are the
	// only thing that makes the second attempt different from the first.
	//
	// It goes ABOVE the repository state and is framed as the task, because a
	// rejection is not context for the original ticket — it IS the work now.
	if why := returnReason(t); why != "" {
		b.WriteString("THIS TICKET WAS SENT BACK BY THE REVIEWER. Your job this time is to fix what it found.\n")
		b.WriteString("The change is already written and pushed; do not start over, correct it.\n\n")
		b.WriteString(clip(why, 2000) + "\n\n")
	}

	// AND WHY THE DEVELOPER COULD NOT USE IT. Same reasoning as the reviewer's
	// send-back above, for the other direction of the round trip: the author is
	// re-queued on a ticket that looks untouched, so without this it writes the
	// specification again from the ticket text and reproduces the fault it was
	// sent back for. "Do not start over" is the operative instruction — the r68
	// author replaced all 215 lines both times rather than fixing one of them.
	if why := specRepairReason(t); why != "" {
		b.WriteString("THE DEVELOPER SENT THIS SPECIFICATION BACK. Your job this time is to CORRECT the\n")
		b.WriteString("tests you already wrote — they are on the branch and the developer may not edit\n")
		b.WriteString("them. Fix the fault named below and change nothing else; do not rewrite the file\n")
		b.WriteString("from scratch, and do not weaken an assertion to make the fault go away.\n\n")
		b.WriteString(clip(why, 2000) + "\n\n")
	}

	b.WriteString("REPOSITORY FILES:\n")
	for _, f := range s.tree {
		fmt.Fprintf(&b, "  %s\n", f)
	}

	if len(s.read) > 0 {
		b.WriteString("\nFILES YOU HAVE READ:\n")
		for _, p := range sortedKeys(s.read) {
			fmt.Fprintf(&b, "\n--- %s ---\n%s\n", p, numberLines(s.read[p]))
		}
	}

	if len(s.staged) > 0 {
		b.WriteString("\nFILES YOU HAVE ALREADY CHANGED (staged, not yet committed):\n")
		for _, p := range sortedKeys(s.staged) {
			fmt.Fprintf(&b, "  %s\n", p)
		}
		// Reading a staged file back is the specific waste this prevents: the
		// contents above are ALREADY the staged version, so a read to "check the
		// write landed" returns exactly what the model just wrote and teaches it
		// nothing, while costing one of very few turns.
		b.WriteString("The contents shown above are the staged versions — reading them again returns your own writes.\n")

	}

	return b.String()
}

// renderDevProgress is the half that changes every turn: where the agent is in
// its budget, what it has already tried, and what came of the last action.
//
// It goes LAST so that everything before it can be cached, and because the model
// reads the end of the prompt most closely — the rejection and the call to act
// are the two things it must not miss. The trail moved down here from above the
// repository listing; it was placed there to describe the agent before the world,
// which is a real distinction but not one worth re-processing the whole tree for
// on every turn.
func renderDevProgress(s *devState) string {
	var b strings.Builder
	// Show the budget, not just the counter. Without a denominator the model has
	// no way to know turns are scarce, and the failure that produced is specific
	// and repeatable: read one file, read another, read another, until the
	// attempt ends having never run the tests. Naming the remainder every turn is
	// what makes "read everything at once" the obviously correct move.
	// NO RESERVE. It was tried and the model ignored it: every reserve turn in a
	// live run went to read_files, not one to run_tests or give_up, so the notice
	// changed the prompt and nothing else. Advice arriving at turn fifty-one does
	// not rescue an agent that has spent fifty turns not converging.
	if s.budget > 0 {
		left := s.budget - s.iteration
		if left < 0 {
			left = 0
		}
		fmt.Fprintf(&b, "ITERATION %d OF %d — %d action(s) left after this one, then the ticket is abandoned.\n\n",
			s.iteration, s.budget, left)
	} else {
		fmt.Fprintf(&b, "ITERATION %d\n\n", s.iteration)
	}

	// What the agent has already done, before what the repository contains.
	//
	// Every other section describes the WORLD; this is the only one that describes
	// the agent. Without it each turn is the model's first turn as far as it can
	// tell, and the observed consequence was an attempt that opened with the same
	// three-file read twice in a row and then re-read a file it had itself just
	// written. A model cannot avoid repeating an action it cannot see it took.
	if len(s.trail) > 0 {
		b.WriteString("ACTIONS YOU HAVE ALREADY TAKEN (do not repeat one — it will cost a turn and change nothing):\n")
		for i, step := range s.trail {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, step)
		}
		b.WriteString("\n")
	}

	// The verification result and the rejection are rendered SEPARATELY. They
	// shared a field once, so a rejection erased the passing test output and the
	// model lost the evidence it needed to justify finishing.
	if s.lastTest != "" {
		status := "FAILED"
		if s.testsPass {
			status = "PASSED"
		}
		fmt.Fprintf(&b, "\nLAST VERIFICATION (%s):\n%s\n", status, s.lastTest)
		// SAY WHAT TO DO ABOUT IT. Every other state in this prompt names the next
		// action — an unverified write is told to run_tests, a refusal is told to
		// write or give up — and success was the one case left implicit, reporting
		// that the checks passed and trusting the model to infer that finishing
		// follows. It does not infer it: across 3,498 recorded turns the agents
		// called finish once, and one ran to iteration 439 still reading.
		//
		// That statistic mostly measures this loop rather than the models, because
		// autoFinishOnGreen normally ends the stage before a passing state can even
		// be shown. But where the opportunity does exist, the instruction should
		// exist too.

	}

	// THE RESTART BRIEF GOES LAST AND ON ITS OWN. It is the most important thing
	// on the page for the agent reading it, and it is not a rejection — the agent
	// being briefed did not take the action that came before.
	if s.restart != "" {
		fmt.Fprintf(&b, "\n%s\n", s.restart)
	}

	if s.notice != "" {
		fmt.Fprintf(&b, "\nYOUR LAST ACTION WAS NOT ACCEPTED:\n%s\n", s.notice)
	}

	b.WriteString("\nReply with one JSON action.")
	return b.String()
}

// parseDevAction decodes and validates one action.
//
// Validation happens HERE rather than at the point of use so an invalid action
// is fed back to the model as an error it can correct, instead of failing the
// task or — worse — being partially executed.
// fileBlockOpen / fileBlockClose delimit file content carried OUTSIDE the JSON.
//
// Source code inside a JSON string has to escape every quote, and Go is the worst
// case for it: a struct tag is `json:"id"`, so a type definition is almost all
// quotes. Observed live — a ticket whose whole job was defining one struct failed
// four replies running on `invalid character '"' after object key:value pair`,
// and took six dependent tickets down with it.
//
// Escaping is not something to ask a model to get right hundreds of characters at
// a time, and neither is base64: the model would have to encode by hand, where a
// single wrong character silently corrupts the file rather than failing loudly.
// Out-of-band delimiters remove the problem instead of moving it — there is
// nothing to escape, so there is nothing to get wrong.
// parseDevReply turns a model reply into one action.
//
// TOOL CALLS FIRST. The department offers the loop's actions as functions, so a
// well-behaved backend answers with a typed call whose arguments the server has
// already validated — no envelope for the model to author, no field for it to
// put in the wrong place. Everything below the tool-call branch is the fallback
// for a backend or model that answers with content anyway.
//
// The <<<FILE block form that used to live here is GONE. It existed to keep Go
// source out of a JSON string, and it cost more than it saved: models put the
// marker inside the JSON, or emitted the JSON without the block, and both wrote
// silent nonsense. Search/replace edits are small enough that ordinary JSON
// escaping is not the problem it was for whole files.
func parseDevReply(res ChatResult, mode agentMode) (devAction, error) {
	if len(res.Calls) > 0 {
		return actionFromCall(res.Calls[0], mode)
	}
	// A TOOL CALL EMITTED AS TEXT is still a tool call. Not every model's chat
	// template is parsed into tool_calls by the server — qwen2.5-coder writes
	// {"name":…,"arguments":{…}} inside <tools> tags and llama.cpp hands it back
	// as content — and refusing it would mean rejecting a model that did exactly
	// what it was asked, for a reason it has no way to see.
	if call, ok := toolCallFromText(res.Content); ok {
		return actionFromCall(call, mode)
	}
	return parseDevAction(res.Content)
}

// actionFromCall maps a tool call onto the loop's action.
func actionFromCall(call ToolCall, mode agentMode) (devAction, error) {
	name := strings.TrimSpace(call.Name)
	allowed := actionsFor(mode)
	if !contains(allowed, name) {
		return devAction{}, fmt.Errorf("unknown tool %q; must be one of %s", name, strings.Join(allowed, ", "))
	}
	act := devAction{Action: name}
	args := strings.TrimSpace(call.Arguments)
	if args == "" || args == "null" {
		return act, nil
	}
	if err := decodeModelJSON(args, &act); err != nil {
		return devAction{}, fmt.Errorf("tool %s: arguments are not valid JSON: %w", name, err)
	}
	act.Action = name // arguments must never redefine which tool was called
	return act, nil
}

// parseDevAction is the content fallback: a bare JSON object naming the action.
func parseDevAction(raw string) (devAction, error) {
	s := strings.TrimSpace(raw)
	// A REPLY THAT ALREADY IS A JSON OBJECT IS NEVER FENCE-EXTRACTED, for the
	// reason set out on decodeJSONObject: extractFenced takes the FIRST ``` in the
	// string, which is right when a model wrapped its answer in a code block and
	// destructive when the answer CONTAINS one. This path carries file contents —
	// a markdown file, a README, a doc comment with an example in it — so a fence
	// inside the payload is ordinary, and extracting it returns the payload's
	// snippet and throws the action away. It surfaced on the architect as
	// "invalid character ']' after top-level value" against a reply that was
	// perfectly well formed.
	if !strings.HasPrefix(s, "{") {
		if fenced := extractFenced(s); fenced != "" {
			s = fenced
		}
	}
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start == -1 || end == -1 || end < start {
		return devAction{}, errors.New("no JSON object in the reply")
	}

	var act devAction
	if err := decodeModelJSON(s[start:end+1], &act); err != nil {
		return devAction{}, fmt.Errorf("reply is not valid JSON: %w", err)
	}
	act.Action = strings.TrimSpace(act.Action)
	if act.Action == "" {
		return devAction{}, errors.New(`reply has no "action" field`)
	}
	if !contains(devActions, act.Action) && act.Action != actionFinish {
		return devAction{}, fmt.Errorf("unknown action %q; must be one of %s", act.Action, strings.Join(devActions, ", "))
	}
	return act, nil
}

func renderDevSummary(branch, summary string, files []string, runID, testOutput string) string {
	return renderStageSummary(modeDevelop, branch, summary, files, runID, testOutput)
}

// renderStageSummary reports what a stage pushed.
//
// The MODE decides the marker and the claim made about the branch, and getting
// that wrong is not cosmetic. The test author used to borrow this whole comment,
// which said "the repository's own pipeline passed on this branch" — false by
// construction for a stage whose tests are supposed to fail, and it set
// branchMarker, which is what hasBranch reads. A tests-only branch therefore
// looked to the reviewer and the integrator exactly like finished work, so a
// ticket whose developer never ran could still have been reviewed and merged.
func renderStageSummary(mode agentMode, branch, summary string, files []string, runID, testOutput string) string {
	sort.Strings(files)
	var b strings.Builder
	switch mode {
	case modeTest:
		b.WriteString(testsWrittenMarker + "\n\n")
	case modeCoverage:
		b.WriteString(coverageMarker + "\n\n")
	default:
		b.WriteString(branchMarker + "\n\n")
	}
	if summary != "" {
		fmt.Fprintf(&b, "%s\n\n", clip(summary, maxSummaryRunes))
	}
	fmt.Fprintf(&b, "- **Branch:** `%s`\n", branch)
	if runID != "" {
		fmt.Fprintf(&b, "- **Pipeline run:** `%s`\n", runID)
	}
	b.WriteString("- **Files changed:**\n")
	for _, f := range files {
		fmt.Fprintf(&b, "  - `%s`\n", f)
	}
	if mode == modeCoverage {
		b.WriteString("\nThese tests were added AFTER the code, for the branches the specification could " +
			"not have known about. They pass, and the suite reached its coverage target.\n")
	} else if mode == modeTest {
		b.WriteString("\nThese tests are the SPECIFICATION for this ticket, and they do NOT pass yet — the code they " +
			"describe has not been written. That is what this stage is for. The developer is next, and its job is " +
			"to make them pass; it cannot edit them.\n")
	} else {
		b.WriteString("\nThe repository's own pipeline passed on this branch — the same one a person's push runs. ")
		b.WriteString("Review and merge as you would a person's branch: the pipeline is the gate, not this comment.\n")
	}
	if testOutput != "" {
		fmt.Fprintf(&b, "\n<details><summary>Verification</summary>\n\n```\n%s\n```\n</details>\n", clip(testOutput, 3000))
	}
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// devActionSchema constrains the developer agent's reply to a well-formed action.
//
// It describes the ENVELOPE only. File content travels in <<<FILE blocks outside
// the JSON, so the schema never has to admit a large free-text string — which is
// what keeps the grammar cheap and, more to the point, keeps source code out of a
// place where it would need escaping at all. The two mechanisms are complementary:
// the schema guarantees the reply parses, the blocks guarantee the code survives.
//
// additionalProperties is false so an invented field is impossible rather than
// silently ignored, and "action" is an enum so an unknown action — one of the
// failure modes the loop used to spend a whole turn rejecting — cannot be emitted.
func devActionSchema() *ReplySchema {
	// ONE SHAPE PER ACTION, because a single object with everything optional is not
	// a constraint at all.
	//
	// This was one flat object requiring only "action", which was harmless while it
	// was a fallback and became the whole contract when the dev turn stopped
	// offering tools. The tool definitions required edits, summary and type on
	// write_files; this copy required none of them, so the grammar happily admitted
	// {"action":"write_files"} — 25 refusals of "write_files with no edits" in the
	// first window after the switch, every one of them schema-valid.
	//
	// EVERY FIELD IS BOUNDED, because this becomes a GRAMMAR and an unbounded string
	// in a grammar is an unbounded reply. Measured when a backend without tool
	// support first fell through to here: the model filled "type" with
	// "replace_all_content_in_file_if_exists_..." and kept going for 5000 tokens and
	// 348 seconds — schema-valid the whole way, because nothing said how long a
	// string may be.
	only := func(action string) map[string]any {
		return map[string]any{"type": "string", "enum": []string{action}}
	}
	return &ReplySchema{
		Name: "dev_action",
		Schema: map[string]any{
			"oneOf": []any{
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"action": only(actionReadFiles),
						"paths": map[string]any{
							"type":     "array",
							"items":    map[string]any{"type": "string", "maxLength": 200},
							"maxItems": maxReadPaths,
							"minItems": 1,
						},
					},
					"required":             []string{"action", "paths"},
					"additionalProperties": false,
				},
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"action":  only(actionWriteFiles),
						"edits":   editsSchema(),
						"summary": map[string]any{"type": "string", "maxLength": maxSummaryRunes},
						"type":    map[string]any{"type": "string", "enum": conventionalTypes},
					},
					// The three the tool form required. An edit-less write is not a
					// smaller edit, it is a wasted turn.
					"required":             []string{"action", "edits", "summary", "type"},
					"additionalProperties": false,
				},
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"action": only(actionUndoEdit),
						"reason": map[string]any{"type": "string", "maxLength": maxSummaryRunes},
					},
					"required":             []string{"action"},
					"additionalProperties": false,
				},
			},
		},
	}
}

// editsSchema describes a batch of line-addressed edits. Shared by the tool
// definition and the content fallback so the two cannot describe different
// shapes — which is how the last format ended up with two channels that
// disagreed.
func editsSchema() map[string]any {
	// ONE SHAPE PER WAY OF POINTING AT THE CODE, because putting every address in
	// one object is what made the copy inevitable.
	//
	// With old_str and replace both required and adjacent, a constrained sampler
	// walks the fields in order and the highest-probability continuation after a
	// long quote is that same quote again. Measured directly: under the flat shape
	// the model sent old_str identical to replace in 39 of 47 turns, and the
	// no-progress ceiling then killed the ticket — twice, taking the board with it.
	// A message naming the mistake did not move it, and neither did a length cap.
	//
	// Branching removes the opportunity rather than discouraging it. The decl shape
	// has no old_str to copy; the anchor shape holds a SHORT quote that cannot be a
	// duplicate of a long replacement; the line shape carries no quote at all.
	path := map[string]any{"type": "string", "maxLength": 200,
		"description": "Repository-relative path to edit."}
	replace := map[string]any{"type": "string",
		"description": "The new text. This is the only place new code goes."}
	return map[string]any{
		"type":     "array",
		"minItems": 1,
		"maxItems": maxWriteFiles,
		"items": map[string]any{
			"oneOf": []any{
				// PREFERRED: name a whole declaration and give its new body once.
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": path,
						"decl": map[string]any{"type": "string", "maxLength": 120,
							"description": "The declaration to replace whole: \"main\", \"apiTasksHandler\", \"Store.Add\"."},
						"replace": replace,
					},
					"required":             []string{"path", "decl", "replace"},
					"additionalProperties": false,
				},
				// A SHORT ANCHOR for a change inside a declaration. Capped here, where
				// the cap is safe because the other two shapes cover what it excludes.
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": path,
						"old_str": map[string]any{"type": "string", "maxLength": 400,
							"description": "A SHORT unique snippet, at most a few lines, copied exactly " +
								"from the numbered contents. It marks WHERE to change and must appear once."},
						"replace": replace,
					},
					"required":             []string{"path", "old_str", "replace"},
					"additionalProperties": false,
				},
				// Line numbers, for picking one of several identical lines, and for
				// creating a file (start_line 0).
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":       path,
						"start_line": map[string]any{"type": "integer", "minimum": 0},
						"end_line":   map[string]any{"type": "integer", "minimum": 0},
						"replace":    replace,
					},
					"required":             []string{"path", "start_line", "end_line", "replace"},
					"additionalProperties": false,
				},
			},
		},
	}
}

// devTools offers the loop's actions as callable functions.
//
// ONE TOOL PER ACTION, rather than one tool with an action enum, because that is
// what the models are trained on and it lets each action carry only its own
// arguments — "edits" is required on write_files and cannot be omitted, which
// was previously a silent empty write.
//
// The descriptions carry the rules that used to live only in the system prompt.
// A rule stated where the model is choosing arguments is followed more often
// than the same rule stated two thousand tokens earlier.
func devTools(mode agentMode) []Tool {
	editRule := "Edit the implementation. You may NOT edit *_test.go files."
	switch mode {
	case modeTest:
		editRule = "Write the tests. You may ONLY edit *_test.go files."
	case modeCoverage:
		editRule = "Add tests. You may ONLY create NEW *_test.go files — the tests that were " +
			"here before you are the specification and cannot be edited."
	}
	// FILTERED THROUGH devActions so the offered tools and the accepted ones cannot
	// drift apart. run_tests, finish and give_up still have definitions below and
	// are simply no longer listed — keeping them costs nothing and makes restoring
	// one a one-line change, whereas a tool offered but not accepted is a trap of
	// exactly the kind this whole change removes.
	all := []Tool{
		{
			Name: actionReadFiles,
			Description: "Read files from the repository. Name every file you need in ONE call — " +
				"each call costs an iteration and you have few.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"paths": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Repository-relative paths, up to 12.",
					},
				},
				"required":             []string{"paths"},
				"additionalProperties": false,
			},
		},
		{
			Name: actionWriteFiles,
			Description: "Edit files. " + editRule +
				" ADDRESS AN EDIT BY ITS TEXT: put the exact snippet you are replacing in \"old_str\" " +
				"(it must appear exactly once) and the new text in \"replace\". To rewrite a whole " +
				"function or type, name it in \"decl\" instead and give the whole declaration in " +
				"\"replace\". Line numbers are a last resort for picking one of several identical " +
				"lines. NEVER put the same text in old_str and replace — old_str is what is there " +
				"now, replace is what it becomes. For a small file you may send the whole new file " +
				"in \"replace\" with old_str, decl, start_line and end_line all empty or 0. " +
				"What you do not name, you do not change.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"edits":   editsSchema(),
					"summary": map[string]any{"type": "string", "description": "One line describing the change."},
					"type": map[string]any{
						"type":        "string",
						"enum":        conventionalTypes,
						"description": "Conventional Commits type for the commit message.",
					},
				},
				"required":             []string{"edits", "summary", "type"},
				"additionalProperties": false,
			},
		},
		{
			Name: actionUndoEdit,
			Description: "Put the file back to what it was before your last write. Use this the moment " +
				"an edit leaves a file you no longer recognise — once the text on disk has diverged " +
				"from what you expect, neither old_str nor a line number will find what you are " +
				"looking for, and further edits make it worse. Undo, read the file, then try again.",
			Parameters: map[string]any{
				"type": "object", "properties": map[string]any{}, "additionalProperties": false,
			},
		},
		{
			Name:        actionRunTests,
			Description: runTestsDescription(mode),
			Parameters: map[string]any{
				"type": "object", "properties": map[string]any{}, "additionalProperties": false,
			},
		},
		{
			Name: actionFinish,
			Description: "Not offered: the stage ends by itself when its checks pass. Kept so restoring " +
				"it is a one-line change to devActions.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"summary": map[string]any{"type": "string", "description": "One line describing the change."},
					"type":    map[string]any{"type": "string", "enum": conventionalTypes},
				},
				"required":             []string{"summary", "type"},
				"additionalProperties": false,
			},
		},
		{
			Name:        actionGiveUp,
			Description: "Stop, explaining why this ticket cannot be done. Use this rather than guessing.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"reason": map[string]any{"type": "string"},
				},
				"required":             []string{"reason"},
				"additionalProperties": false,
			},
		},
	}
	allowed := actionsFor(mode)
	out := make([]Tool, 0, len(all))
	for _, t := range all {
		if contains(allowed, t.Name) {
			out = append(out, t)
		}
	}
	return out
}

func runTestsDescription(mode agentMode) string {
	if mode == modeCoverage {
		return "Run the suite and report statement coverage. The suite must pass and coverage must " +
			"reach the target before you finish."
	}
	if mode == modeTest {
		return "Push your tests and check they are VALID GO. It does not run them — they cannot pass " +
			"yet, because the code they describe does not exist."
	}
	return "Push the branch and run the repository's own pipeline against it. Must pass before you finish."
}

// returnReason extracts the review that most recently sent this ticket back, so
// the developer agent is told what to fix rather than being handed the original
// ticket a second time.
//
// The LAST such comment wins, not the first: a ticket can be returned more than
// once, and the finding that matters is the one from the round just finished.
// Everything before it has either been fixed or superseded, and quoting all of
// them would spend the prompt re-litigating rounds that are over.
func returnReason(t Ticket) string {
	return latestMarkedComment(t, returnedMarker)
}

// specRepairReason is the developer's hand-back, for the author that has to act
// on it.
//
// THE HAND-BACK WAS WRITE-ONLY UNTIL NOW. endOnBrokenSpec posts a comment naming
// the file, the kind of fault and the failing output, and specRepairsSoFar counts
// those comments to bound the round trips — so the mechanism looked complete and
// the ceiling worked. Nothing ever rendered the BODY into a prompt, because
// returnReason matched the reviewer's marker alone.
//
// Measured on r68: a section panicked in its own fixture (an unescaped space in a
// query string), the developer handed it back twice, and both times the author
// opened with "I'll write the unit tests for the status parameter filtering
// functionality" — it had no idea it was a repair. It rewrote the whole file from
// scratch and reproduced the identical bug, twice, until the ceiling stopped it.
// The round trip cannot converge while the reason for it is unreadable.
func specRepairReason(t Ticket) string {
	return latestMarkedComment(t, specRepairMarker)
}

// latestMarkedComment returns the newest comment carrying marker, stripped of the
// marker and the token/latency footer.
func latestMarkedComment(t Ticket, marker string) string {
	var latest Comment
	var found bool
	for _, c := range t.Comments {
		if !strings.Contains(c.Body, marker) {
			continue
		}
		if !found || c.CreatedAt.After(latest.CreatedAt) ||
			(c.CreatedAt.Equal(latest.CreatedAt) && c.CommentID > latest.CommentID) {
			latest, found = c, true
		}
	}
	if !found {
		return ""
	}
	// Strip the machine marker and the token/latency footer: neither is something
	// to act on, and the footer in particular reads as data the agent might try to
	// address.
	body := strings.ReplaceAll(latest.Body, marker, "")
	if i := strings.Index(body, "<sub>"); i >= 0 {
		body = body[:i]
	}
	return strings.TrimSpace(body)
}

// systemPrompt selects the brief for this agent's mode.
// The ticket is a parameter rather than a field because one agent serves many
// tickets concurrently, and which job this is belongs to the ticket.
func (a *DevAgent) systemPrompt(t Ticket) string {
	switch a.mode {
	case modeSpecMerge:
		// Which job this is comes from the ticket, not the mode: the same stage
		// reconciles collisions and repairs a hand-back.
		return specMergeSystemPrompt(specRepairsSoFar(t) > 0)
	case modeTest:
		return testerSystemPrompt()
	case modeCoverage:
		return coverageSystemPrompt(a.repo.CoverageTarget)
	}
	return devSystemPrompt()
}

// coverageSystemPrompt briefs the second test stage — the one that HAS seen the
// code.
//
// Its whole value is the thing the spec author was denied: the implementation is
// in front of it, so it can find the branches the ticket never mentioned. The
// brief therefore points it at the code rather than at the ticket, which is the
// exact inverse of the earlier stage and the reason they are two stages.
//
// The prohibition is stated twice — here and enforced in applyEdits — because it
// is the one that matters. Raising a coverage number by weakening an existing
// test hits the target while destroying what the target stands for, and it is
// the cheapest move available.
func coverageSystemPrompt(target int) string {
	return fmt.Sprintf(`You add tests to code that is already written and already passing.

A different agent wrote the specification tests from the ticket, before any code existed.
Those tests pass. Your job is what they could not do: they had never seen the implementation,
so they cannot have covered the branches it actually grew.

Call ONE tool per turn.

YOUR TARGET IS %d%% STATEMENT COVERAGE, and the suite must keep passing.

HOW TO FIND WHAT IS MISSING. Read the implementation and look for lines a test would not
reach:
- error paths — what happens when a dependency returns an error?
- empty and nil inputs, empty collections, zero values
- boundary values — the limit itself, one below, one above
- branches taken only by unusual input: a malformed body, a missing field, a duplicate id
- early returns and guard clauses

HOW EDITING WORKS. Each edit names the EXACT text to replace and what to replace it with.
To CREATE a file, omit start_line and end_line and put the whole file in "replace".

Rules:
- YOU MAY ONLY ADD NEW TEST FILES. The tests that were here before you are the specification
  the developer was held to. Editing them is refused — raising coverage by weakening an
  existing test would hit the number and destroy what it measures. Put your tests in a new
  file, for example coverage_test.go.
- You may NOT change the implementation. If a branch cannot be reached, say so in a comment
  in your test file rather than making the code reachable.
- Every test you add must ASSERT something. A test that calls a function and checks nothing
  raises coverage and catches no bug, which is the failure this whole stage exists to avoid.
- Use only the standard library and the repository's existing dependencies.
- STOP THE TEST WHEN AN ASSERTION MAKES THE REST MEANINGLESS. If you assert a length, a
  count, or that an error is nil, and the lines after it index or dereference that value,
  use require (require.Len, require.NoError, require.NotNil) — NOT assert. assert records
  the failure and CARRIES ON, so the next line indexes an empty slice and the test panics.
  The panic then hides the assertion that actually explains the fault behind twenty lines
  of runtime stack, and the developer reads a crash instead of "expected 2 items, got 0".
  Use assert for a check the following lines do not depend on; require for one they do.
- THE TESTS RUN THEMSELVES after every write that changes something, and report the coverage
  figure. Keep adding until it reaches the target; the stage ends by itself when it does.
- Your iterations are counted. The state below says how many remain.`, target)
}

// testerSystemPrompt briefs the test author.
//
// It shares the developer's action vocabulary — same loop, same JSON, same file
// blocks — because the mechanics are identical and a second dialect would be a
// second parser to keep correct. What it replaces is the JOB, and the two things
// it has to establish are the ones this stage exists for:
//
//   - the tests come from the TICKET, not from any implementation. There is no
//     implementation to read; that is the point, and the prompt says so plainly
//     so the model does not go looking for one and conclude the repository is
//     broken;
//   - RED IS SUCCESS HERE. Every instinct in a coding model says a failing test
//     is a problem to fix, and the one fix available — weakening the test until
//     it passes — is precisely the outcome this stage was introduced to prevent.
//     So the prompt states the inversion directly rather than hoping.
func testerSystemPrompt() string {
	return `You write the unit tests for a ticket, BEFORE the implementation exists.

You are not the developer. A different agent will write the code afterwards, and its
job is to make your tests pass. Your tests are the specification it works to.

Call ONE tool per turn. The tools are described in your tool list; this is the context
you need to use them well.

HOW EDITING WORKS. To CREATE a test file, omit start_line and end_line and put the
entire file in "replace". To change a test file you have already written, either give
start_line and end_line from the numbered contents shown to you — 1-indexed and
inclusive, and do not repeat the numbers inside "replace" — or omit them and send the
whole corrected file, which for your own test files is usually simpler.

What makes a good test here:
- IF THE TICKET LISTS "This ticket is done when...", THOSE CRITERIA ARE THE WHOLE JOB.
  Write a test for each one and STOP. Prefer one test function per criterion, using
  subtests for its cases; the developer must satisfy everything you write, so every
  extra function is work the ticket did not ask for. They were drawn from the request by the stage
  before you; anything not in that list is not part of this ticket.
- Test what the TICKET asks for, clause by clause. Every stated rule — a status code, a
  length limit, an error case, a filter — should have a test that would fail if it were
  missing. Work from the ticket text, not from any code you happen to see.
- DO NOT INVENT REQUIREMENTS. A test you add because it seemed sensible is a rule the
  developer must satisfy and may not change. Measured on real tickets: assertions that
  a list is empty rather than nil, that a title search ignores case, that ids run in
  sequence — none of them requested by anyone — each cost a developer its entire budget.
  If the ticket does not say it, do not assert it.
- Name the functions and types the ticket implies, and use them as if they exist. You are
  DEFINING the interface the developer must build. Prefer the obvious name.
- Cover the error and boundary cases, not just the happy path. A test suite that only
  proves the easy case is the one that lets a broken change through.
- Use only the standard library and the repository's existing dependencies.
- ONE PACKAGE PER DIRECTORY, AND YOU CHOOSE IT FOR EVERYONE. Go allows a directory to
  hold exactly one package name. Declare the SAME package as the .go files already in
  that directory — read one and copy its package line — and only pick a new name if the
  directory has none. The developer cannot correct this: it may not edit your tests, so
  whatever you declare is what every file beside yours must also declare. Measured on
  r75: an author wrote "package api" into a directory whose other files said
  "package store", the whole tree stopped compiling with "found packages api (api.go)
  and store (store.go)", and the developer spent 24 turns with no legal move.
- STOP THE TEST WHEN AN ASSERTION MAKES THE REST MEANINGLESS. If you assert a length, a
  count, or that an error is nil, and the lines after it index or dereference that value,
  use require (require.Len, require.NoError, require.NotNil) — NOT assert. assert records
  the failure and CARRIES ON, so the next line indexes an empty slice and the test panics.
  The panic then hides the assertion that actually explains the fault behind twenty lines
  of runtime stack, and the developer reads a crash instead of "expected 2 items, got 0".
  Use assert for a check the following lines do not depend on; require for one they do.

Rules:
- YOUR TESTS WILL NOT COMPILE OR PASS YET. That is correct and expected: the functions
  they call have not been written. Do NOT write stubs, placeholder implementations, or
  trivially-true assertions to make anything pass. Do NOT weaken a test. A failing test
  against absent code is exactly what this stage is supposed to produce.
- DO NOT DECLARE THE IMPLEMENTATION IN YOUR TEST FILE. Never write "type Task struct",
  "func NewStore()" or similar in a *_test.go. Those belong to the developer, and it is
  not allowed to edit your files — so anything you declare, it can never write. Call the
  types and functions as if they already existed; the compile error that produces is the
  whole point. Unexported test scaffolding (a testCase struct, a setup helper) is fine.
- AN EMPTY TEST BODY IS REFUSED, AND SO IS t.Skip. A test function containing only
  comments, or only t.Skip("Not implemented"), is not a test — it compiles, it passes,
  and it specifies nothing. Never skip a test you are supposed to be writing. Both
  gates you must clear check for this: every Test function must contain statements, and
  the suite must FAIL. If your tests pass, you have not specified anything and the stage
  will send them back.
- You may ONLY edit *_test.go files. You cannot write the implementation. If you think the
  ticket is impossible, say so in a comment in your test file rather than testing something
  easier.
- EDIT BY LINE NUMBER. The files shown to you are numbered; give start_line and end_line
  (1-indexed, inclusive) and the new text for that range in "replace", WITHOUT the line
  numbers. For one of your own test files you may also omit both and put the entire
  corrected file in "replace", which is often the simpler move.
- THE TESTS RUN THEMSELVES after every write that changes something, and you are shown the
  result on your next turn. You do not ask for this, and you do not finish: the stage ends
  by itself once your tests are well-formed and failing as they should.
- Paths are relative to the repository root. Absolute paths and ".." are refused.
- Your iterations are counted and few. The state below says how many remain. A normal
  ticket is two turns: read what you need, then write the tests.`
}

// toolCallWrappers are the tags models put around a tool call they wrote as
// text. Stripped rather than matched exactly, because each family invents its
// own and the contents are the same either way.
var toolCallWrappers = []string{"tools", "tool_call", "tool_calls", "function_call"}

// toolCallFromText recovers a tool call a model wrote into its content.
//
// The shape is the one the tool-calling API itself uses — a name and an
// arguments object — which is what distinguishes it from the hand-written
// envelope parseDevAction reads. Both are accepted because both are things
// models actually emit; neither is preferred by taste.
func toolCallFromText(content string) (ToolCall, bool) {
	s := strings.TrimSpace(content)
	if s == "" {
		return ToolCall{}, false
	}
	for _, tag := range toolCallWrappers {
		if i := strings.Index(s, "<"+tag+">"); i >= 0 {
			rest := s[i+len(tag)+2:]
			if j := strings.Index(rest, "</"+tag+">"); j >= 0 {
				rest = rest[:j]
			}
			s = strings.TrimSpace(rest)
			break
		}
	}
	if fenced := extractFenced(s); fenced != "" {
		s = fenced
	}
	start, end := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if start < 0 || end < start {
		return ToolCall{}, false
	}

	var raw struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := decodeModelJSON(s[start:end+1], &raw); err != nil {
		return ToolCall{}, false
	}
	if raw.Name == "" {
		return ToolCall{}, false // an envelope, not a tool call
	}
	args := strings.TrimSpace(string(raw.Arguments))
	if args == "" || args == "null" {
		args = "{}"
	}
	return ToolCall{Name: raw.Name, Arguments: args}, true
}

// specRepairSystemPrompt briefs the same stage on its OTHER job: a specification
// the developer could not satisfy.
//
// The failure itself is rendered into the state — see specRepairReason — so this
// says what the job IS, which the reconciler's brief actively denies.
const specRepairSystemPrompt = `You are fixing a specification the developer could not satisfy.

You wrote these tests, or agents like you did. A developer then tried to write code
that makes them pass, and reported that no code could. You are the only stage allowed
to edit them, so this is yours to correct.

The failure it hit is quoted below. Read it first: it names the test and what went
wrong.

Call ONE tool per turn.

WHAT MAKES A TEST UNSATISFIABLE. These are the shapes that reach you, and each has
one honest fix:
- The test panics inside its own code before any assertion runs — a nil map written
  to, reflect on a type that does not support it, an index past the end of a fixture.
  Fix the test's own setup.
- The test names something no implementation can provide: a package it forgot to
  import, a qualifier like url.QueryEscape with no "net/url" in the import block.
  Add the import.
- Two assertions contradict each other, so satisfying one breaks the other. Decide
  which the ticket actually asks for and correct the other.
- A signature the test calls does not match what it then asserts about the result.

WHAT YOU MUST NOT DO:
- DO NOT WEAKEN THE TEST. Loosening an assertion, deleting a case or emptying a body
  makes the failure go away and removes a requirement with it, and nothing downstream
  can tell that it is missing. Fix the fault; keep the demand.
- DO NOT EDIT THE IMPLEMENTATION. You may not touch non-test files and the attempt
  will be refused. If the failure really is the code's fault, say so in your summary
  and finish — the developer gets it back.
- Do not rewrite a file from scratch. Correct the fault named below and change
  nothing else.

If the tests already look correct to you, do not edit anything: finish and say why.
Repeating an edit that changes nothing costs a turn and moves nothing.`

// specMergeSystemPrompt briefs the reconciler.
//
// It is the only stage that edits tests it did not write, so the brief is mostly
// about what NOT to do. The temptation when two tests collide is to delete one —
// which compiles, passes the gate, and silently removes a requirement the
// developer would otherwise have had to satisfy.
//
// IT HAS TWO JOBS AND THE BRIEF HAS TO SAY WHICH. Reconciling collisions is the
// first. The second arrived when the developer's hand-back was routed here — this
// is the only stage allowed to edit a task's tests, so an unsatisfiable
// specification is its to repair — and for a while the brief still described only
// the first.
//
// Measured on r83, and it cost seventeen minutes: the files compiled perfectly,
// so by a brief that said "YOUR ONLY JOB is to make these files compile TOGETHER,
// nothing else" there was nothing to do. The agent went looking anyway, tried to
// edit api.go three times (refused — it may not touch implementation), and made
// sixteen no-op edits over seventy-six turns.
//
// A stage given a new responsibility and not told about it does not conclude that
// it has nothing to do. It improvises.
func specMergeSystemPrompt(repairing bool) string {
	if repairing {
		return specRepairSystemPrompt
	}
	return `You reconcile a specification that several authors wrote at the same time.

Each of them wrote one section of it into its own file, on this branch, without seeing the
others. The result is one Go package, and it does not compile: two of them chose the same
name for a test, or declared the same helper twice.

Call ONE tool per turn.

YOUR ONLY JOB is to make these files compile TOGETHER. Nothing else.

WHAT TO DO:
- A test function declared twice: rename one after the section its file belongs to, e.g.
  TestConcurrentAccess in store_data_test.go becomes TestStoreData_ConcurrentAccess.
- A helper or type declared twice with the same body: keep one, delete the other.
- A helper declared twice with DIFFERENT bodies: rename one; they are not the same thing.

WHAT YOU MUST NOT DO:
- DO NOT DELETE A TEST. If two tests collide, both stay — one gets a new name. Deleting one
  removes a requirement the developer would have had to satisfy, and nothing downstream can
  tell that it is missing.
- Do not weaken, skip or empty an assertion. Every t.Errorf, t.Fatalf and require call that
  is here now must still be here when you finish.
- Do not add tests. You are not specifying anything; you are making what exists compile.
- Do not touch the implementation. Only *_test.go files.

THE TESTS STILL FAIL WHEN YOU ARE DONE, and that is correct: the code they describe has not
been written yet. What must change is that they fail on ASSERTIONS and undefined symbols,
not on a redeclaration.

Rules:
- Paths are relative to the repository root. Absolute paths and ".." are refused.
- Your iterations are counted and few. Most of this is two or three renames.`
}
