package window

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/transcript"
)

func TestReadThoughtsNewestLast(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeTranscript(t, dir, "2026-08-25",
		transcript.Record{Kind: transcript.KindTurn, TaskID: "t1", Role: "dev-agent",
			Completion: "first I will read the store", At: now.Add(-2 * time.Minute)},
		transcript.Record{Kind: transcript.KindTurn, TaskID: "t1", Role: "dev-agent",
			Completion: "now I will edit it", At: now.Add(-time.Minute)},
	)

	got := readThoughts(dir, "t1")
	if len(got) != 2 {
		t.Fatalf("got %d thoughts, want 2", len(got))
	}
	if !strings.Contains(got[1].Prose, "edit it") {
		t.Fatalf("newest is last; got[1] = %q", got[1].Prose)
	}
	if got[0].Role != "dev-agent" {
		t.Fatalf("Role = %q, want dev-agent", got[0].Role)
	}
}

func TestReadThoughtsOnlyTheAskedTicket(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeTranscript(t, dir, "2026-08-25",
		transcript.Record{Kind: transcript.KindTurn, TaskID: "t1", Completion: "mine", At: now},
		transcript.Record{Kind: transcript.KindTurn, TaskID: "t2", Completion: "theirs", At: now},
	)

	got := readThoughts(dir, "t1")
	if len(got) != 1 || got[0].Prose != "mine" {
		t.Fatalf("got %+v, want only t1's turn — the panel is per ticket", got)
	}
}

// THE REASON AN AGENT LOOPS IS IN THE REFUSAL IT KEEPS EARNING. Reading the
// turns without them shows an agent apparently deciding the same thing over and
// over for no reason.
func TestReadThoughtsIncludesRefusals(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeTranscript(t, dir, "2026-08-25",
		transcript.Record{Kind: transcript.KindTurn, TaskID: "t1", Completion: "I will fix the test", At: now.Add(-time.Minute)},
		transcript.Record{Kind: transcript.KindRefusal, TaskID: "t1", Tool: "test_file",
			Detail: "store_test.go is written by the spec author", At: now},
	)

	got := readThoughts(dir, "t1")
	if len(got) != 2 {
		t.Fatalf("got %d, want the turn and the refusal", len(got))
	}
	last := got[1]
	if !last.Failed {
		t.Fatal("the refusal must be marked, or it reads as another ordinary turn")
	}
	if !strings.Contains(last.Prose, "spec author") {
		t.Fatalf("Prose = %q, want the refusal's reason — that is the whole value", last.Prose)
	}
	if last.Tool != "test_file" {
		t.Fatalf("Tool = %q, want the refusal code", last.Tool)
	}
}

func TestReadThoughtsIgnoresOtherKinds(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeTranscript(t, dir, "2026-08-25",
		transcript.Record{Kind: transcript.KindStart, TaskID: "t1", At: now},
		transcript.Record{Kind: transcript.KindAction, TaskID: "t1", Tool: "run", Detail: "go test", At: now},
		transcript.Record{Kind: transcript.KindOutcome, TaskID: "t1", Status: "merged", At: now},
	)

	if got := readThoughts(dir, "t1"); len(got) != 0 {
		t.Fatalf("got %d, want none — this panel is what the model SAID, and an "+
			"action is already on the row", len(got))
	}
}

func TestReadThoughtsCaps(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	var recs []transcript.Record
	for i := 0; i < MaxThoughtsShown+10; i++ {
		recs = append(recs, transcript.Record{Kind: transcript.KindTurn, TaskID: "t1",
			Completion: fmt.Sprintf("turn %d", i), At: now.Add(time.Duration(i) * time.Second)})
	}
	writeTranscript(t, dir, "2026-08-25", recs...)

	got := readThoughts(dir, "t1")
	if len(got) != MaxThoughtsShown {
		t.Fatalf("got %d thoughts, want the cap of %d", len(got), MaxThoughtsShown)
	}
	// THE NEWEST ARE KEPT. Trimming the tail instead would show the opening moves
	// of a stuck run and hide the loop it is in now.
	want := fmt.Sprintf("turn %d", MaxThoughtsShown+9)
	if got[len(got)-1].Prose != want {
		t.Fatalf("last = %q, want %q — the cap must drop the OLDEST", got[len(got)-1].Prose, want)
	}
}

// THE SAME DIAGNOSIS THREE TIMES RUNNING IS THE SIGNATURE THAT MATTERS, so the
// panel has to hold enough turns for a loop to be visible as a loop.
func TestMaxThoughtsShownHoldsARepeat(t *testing.T) {
	if MaxThoughtsShown < 6 {
		t.Fatalf("MaxThoughtsShown = %d — too few to see a diagnosis repeat, which is "+
			"the pattern the panel exists to make visible", MaxThoughtsShown)
	}
}

func TestReadThoughtsWithoutACorpus(t *testing.T) {
	for _, dir := range []string{"", config.TranscriptOff} {
		if got := readThoughts(dir, "t1"); got != nil {
			t.Fatalf("dir %q: want nil, got %d", dir, len(got))
		}
	}
	if got := readThoughts(t.TempDir(), ""); got != nil {
		t.Fatal("no ticket selected means nothing to read")
	}
}

// ---- ProseOf ----

// THE RECORDED REASONING IS THE MODEL'S OWN ACCOUNT. Stripping syntax off the
// completion is a guess; when the endpoint told us what it was thinking, that
// wins.
func TestProseOfPrefersTheRecordedReasoning(t *testing.T) {
	r := transcript.Record{
		Reasoning:  "the router drops the leading slash",
		Completion: `{"tool":"edit","path":"router.go"}`,
	}
	if got := ProseOf(r); got != "the router drops the leading slash" {
		t.Fatalf("ProseOf = %q, want the recorded reasoning", got)
	}
}

func TestProseOfStripsToolCalls(t *testing.T) {
	cases := []struct {
		name, completion, want string
	}{
		{"bare object", `I will edit the router. {"tool":"edit","path":"a.go"}`, "I will edit the router."},
		{"fenced json", "Looking at it now.\n```json\n{\"tool\":\"read\"}\n```", "Looking at it now."},
		{"bare fence", "Trying this.\n```\n{\"tool\":\"read\"}\n```", "Trying this."},
		{"tagged", "Here goes. <tool_call>{\"tool\":\"read\"}</tool_call>", "Here goes."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ProseOf(transcript.Record{Completion: c.completion})
			if got != c.want {
				t.Fatalf("ProseOf = %q, want %q", got, c.want)
			}
		})
	}
}

// A BLANK LINE READS AS A FAILED READ. A turn that carried no prose is itself a
// finding — a model that has stopped explaining itself is usually looping.
func TestProseOfNamesAToolOnlyTurn(t *testing.T) {
	got := ProseOf(transcript.Record{Completion: `{"tool":"read","path":"a.go"}`})
	if !strings.Contains(got, "tool call only") {
		t.Fatalf("ProseOf = %q, want it to say the turn had no prose", got)
	}
}

func TestProseOfEmptyCompletion(t *testing.T) {
	if got := ProseOf(transcript.Record{}); got != "" {
		t.Fatalf("ProseOf = %q, want empty — there was no turn to describe", got)
	}
}

func TestProseOfCollapsesWhitespace(t *testing.T) {
	got := ProseOf(transcript.Record{Completion: "one\n\n\ttwo   three"})
	if got != "one two three" {
		t.Fatalf("ProseOf = %q, want the lines collapsed: the panel wraps to the "+
			"terminal and a model's own line breaks fight that", got)
	}
}

func TestProseOfBounds(t *testing.T) {
	got := ProseOf(transcript.Record{Completion: strings.Repeat("word ", 500)})
	if len([]rune(got)) > 401 {
		t.Fatalf("ProseOf is %d runes; one turn must not fill the panel", len([]rune(got)))
	}
}

// CLIPPING BY BYTE SPLITS A CHARACTER and puts a replacement glyph on screen.
// Measured before on a commit subject; the same arithmetic is here.
func TestProseOfClipsByRuneNotByte(t *testing.T) {
	got := ProseOf(transcript.Record{Reasoning: strings.Repeat("é", 500)})
	if strings.ContainsRune(got, '�') {
		t.Fatal("ProseOf split a multi-byte character")
	}
	if n := len([]rune(strings.TrimSuffix(got, "…"))); n != 400 {
		t.Fatalf("kept %d runes, want 400 — the bound is in runes, not bytes", n)
	}
}

// ---- ToolOf ----

func TestToolOf(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", `{"tool":"edit","path":"a.go"}`, "edit"},
		{"with prose", `I will read it. {"tool":"read"}`, "read"},
		{"fenced", "```json\n{\"tool\":\"verify\"}\n```", "verify"},
		{"absent", "just prose", ""},
		{"empty name", `{"tool":""}`, ""},
		{"truncated", `{"tool":"ed`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ToolOf(c.in); got != c.want {
				t.Fatalf("ToolOf(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestClipRunesDoesNotTrim(t *testing.T) {
	// clipRunes is used on content whose leading space is meaningful, unlike clip.
	if got := clipRunes("  indented", 40); got != "  indented" {
		t.Fatalf("clipRunes = %q, want the indent kept", got)
	}
}
