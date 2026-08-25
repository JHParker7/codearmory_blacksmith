package dev

import (
	"fmt"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/edit"
)

// AN IDENTICAL STEP REPEATED IS ONE FACT, NOT SEVERAL. Writing it out several
// times costs twice: it spends the twenty-step window on one mistake, evicting
// the reads and edits that explain how the agent got there, and it spends prompt
// on redundancy — r74's looping developer reached 12,565 prompt tokens to emit a
// 200-token repeat, most of it seven copies of one refusal.
func TestALoopCollapsesIntoOneLineThatCountsIt(t *testing.T) {
	var s State
	for range 7 {
		s.Remember("write_files(store.go, 40B [ab12cd])", "old_str matched nothing")
	}
	if len(s.History) != 1 {
		t.Fatalf("%d entries for one repeated action:\n%s", len(s.History), strings.Join(s.History, "\n--\n"))
	}
	// A LINE SAYING IT HAS DONE THE SAME THING SEVEN TIMES is a fact about its own
	// behaviour, which is a different thing to read than an instruction — and the
	// agent could already be told not to repeat itself and did anyway.
	if !strings.Contains(s.History[0], "7 times in a row") {
		t.Errorf("the repeat is not counted:\n%s", s.History[0])
	}

	// A DIFFERENT ACTION BREAKS THE RUN rather than extending the count.
	s.Remember("read_files(store.go)", "read 1 file")
	if len(s.History) != 2 {
		t.Fatalf("a different action did not start a new entry: %v", s.History)
	}
	s.Remember("read_files(store.go)", "read 1 file")
	if len(s.History) != 2 || !strings.Contains(s.History[1], "2 times in a row") {
		t.Errorf("the new action does not count its own repeats: %v", s.History)
	}
}

// A SAME-ACTION-DIFFERENT-OUTCOME IS NOT A REPEAT: the outcome is half of what
// the entry records, and collapsing on the action alone would hide a change.
func TestTheOutcomeIsPartOfWhatCountsAsARepeat(t *testing.T) {
	var s State
	s.Remember("run", "failed: undefined Task")
	s.Remember("run", "failed: undefined Store")
	if len(s.History) != 2 {
		t.Errorf("two different outcomes collapsed into one: %v", s.History)
	}
}

// TWENTY IS ENOUGH TO REMEMBER A LINE OF ATTACK and few enough that the slot
// does not fill with old actions. The OLDEST go first — the recent ones are what
// explain where the agent now is.
func TestTheHistoryKeepsTheMostRecentTurns(t *testing.T) {
	var s State
	for i := range MaxHistoryTurns + 10 {
		s.Remember(fmt.Sprintf("action %d", i), "ok")
	}
	if len(s.History) != MaxHistoryTurns {
		t.Fatalf("kept %d turns, want %d", len(s.History), MaxHistoryTurns)
	}
	if !strings.Contains(s.History[len(s.History)-1], fmt.Sprintf("action %d", MaxHistoryTurns+9)) {
		t.Errorf("the newest turn was dropped: %q", s.History[len(s.History)-1])
	}
	if strings.Contains(strings.Join(s.History, "\n"), "action 0\n") {
		t.Error("the oldest turn survived the window")
	}
}

func TestAnEmptyActionIsNotRemembered(t *testing.T) {
	var s State
	s.Remember("", "something happened")
	s.Remember("   ", "something else")
	if len(s.History) != 0 {
		t.Errorf("a nameless action was recorded: %v", s.History)
	}
}

// THE ASSISTANT CHANNEL IS A FORMAT EXAMPLE, WHATEVER IT IS USED FOR.
//
// Replaying raw completions there taught the model to imitate the wire format,
// so the replay became descriptions instead — and it then imitated the
// descriptions, emitting `write_files: store_concurrent_test.go lines 169-169,
// 1B [169]` as a literal reply. Off-schema output is unconstrained output, and
// the collapse followed: nine of one attempt's turns ran to 9,000–17,000
// characters of `}(i)}(i)}(i)` before the sampler was cut off.
func TestTheRecordIsShownAsOneUserMessageAndNeverAsAssistantTurns(t *testing.T) {
	var s State
	s.Remember("read_files(store.go)", "read 1 file")
	s.Remember("write_files(store.go)", "staged 1 file")

	msgs := s.RecentHistory()
	if len(msgs) != 1 {
		t.Fatalf("the record became %d messages; it must be one", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Errorf("the record is in the %q role; anything there is a demonstration "+
			"of what to produce", msgs[0].Role)
	}
	for _, want := range []string{"read_files(store.go)", "write_files(store.go)",
		"Do not repeat an action that was refused"} {
		if !strings.Contains(msgs[0].Content, want) {
			t.Errorf("the record does not carry %q", want)
		}
	}
	// An agent with no history sends nothing rather than an empty heading.
	if got := (&State{}).RecentHistory(); got != nil {
		t.Errorf("an empty history produced %v", got)
	}
}

// THE SYNTAX IS WHAT IT IMITATES; THE PROSE IS WHAT IT REASONED. Keeping the
// second without the first is the whole point of carrying a history.
func TestOnlyTheReasoningIsKeptFromAReply(t *testing.T) {
	cases := map[string]string{
		"a bare object": `The store needs a filter. {"action":"read_files","paths":["store.go"]}`,
		"a fenced one":  "The store needs a filter.\n```json\n{\"action\":\"read_files\"}\n```",
		"a tagged one":  `The store needs a filter. <tool_call>{"name":"read_files"}</tool_call>`,
		"prose only":    "The store needs a filter.",
		"across lines":  "The store needs a filter.\n{\n  \"action\": \"read_files\"\n}",
	}
	for name, reply := range cases {
		got := ProseOf(reply)
		if !strings.Contains(got, "The store needs a filter.") {
			t.Errorf("%s: the reasoning was lost: %q", name, got)
		}
		for _, syntax := range []string{"action", "tool_call", "```"} {
			if strings.Contains(got, syntax) {
				t.Errorf("%s: the syntax survived: %q", name, got)
			}
		}
	}
	if ProseOf("") != "" {
		t.Error("an empty reply produced prose")
	}
}

// THE PATH ALONE MADE EVERY WRITE LOOK THE SAME. The trail tells the agent "do
// not repeat an action", and for a write it recorded only the file name — so
// twenty attempts at twenty different payloads and twenty attempts at the SAME
// payload were indistinguishable in its own history. Measured on r66: 75 writes
// byte-identical to one already accepted, none of them legible as repeats.
func TestTwoDifferentWritesToOneFileAreDistinguishable(t *testing.T) {
	write := func(replace string) Action {
		return Action{Action: ActionWriteFiles, Edits: []edit.Edit{
			{Path: "store.go", StartLine: 10, EndLine: 12, Replace: replace},
		}}
	}
	a := StepDetail(write("func List() {}"))
	b := StepDetail(write("func List() []Task { return nil }"))
	if a == b {
		t.Errorf("two different payloads render identically: %q", a)
	}
	// And the SAME payload renders identically, which is what makes a repeat
	// visible.
	if StepDetail(write("func List() {}")) != a {
		t.Error("the same payload rendered two ways; a repeat would be invisible")
	}
	if !strings.Contains(a, "store.go") || !strings.Contains(a, "lines 10-12") {
		t.Errorf("the detail does not say where the write went: %q", a)
	}

	// A read names its files; anything else carries no detail rather than a
	// misleading one.
	if got := StepDetail(Action{Action: ActionReadFiles, Paths: []string{"a.go", "b.go"}}); got != "a.go, b.go" {
		t.Errorf("read detail = %q", got)
	}
	if got := StepDetail(Action{Action: ActionUndoEdit}); got != "" {
		t.Errorf("undo carries a detail it cannot have: %q", got)
	}
}

// THE HISTORY IS REPLAYED EVERY TURN, so it is charged for on every one of the
// few turns the attempt has. A twelve-file write must not become a page.
func TestAStepDetailIsBoundedHoweverLargeTheAction(t *testing.T) {
	var edits []edit.Edit
	for i := range MaxWriteFiles {
		edits = append(edits, edit.Edit{
			Path:    fmt.Sprintf("some/quite/long/path/to/file_%d.go", i),
			Replace: strings.Repeat("x", 5000),
		})
	}
	got := StepDetail(Action{Action: ActionWriteFiles, Edits: edits})
	if len([]rune(got)) > 261 {
		t.Errorf("a write rendered %d runes into the per-turn history", len([]rune(got)))
	}

	long := make([]string, 40)
	for i := range long {
		long[i] = fmt.Sprintf("some/quite/long/path/to/file_%d.go", i)
	}
	if n := len([]rune(StepDetail(Action{Action: ActionReadFiles, Paths: long}))); n > 161 {
		t.Errorf("a read rendered %d runes into the per-turn history", n)
	}
}

func TestAStepReadsAsItsActionAndArguments(t *testing.T) {
	if got := (Step{Action: "read_files", Detail: "a.go"}).String(); got != "read_files(a.go)" {
		t.Errorf("Step.String() = %q", got)
	}
	// No detail means no empty parentheses.
	if got := (Step{Action: "undo_edit"}).String(); got != "undo_edit" {
		t.Errorf("Step.String() = %q", got)
	}
}
