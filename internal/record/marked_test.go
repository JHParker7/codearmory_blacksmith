package record

import (
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/ticket"
)

// THE LAST ONE WINS, NOT THE FIRST. A ticket can be sent back more than once,
// and the finding that matters is the one from the round just finished:
// everything before it has been fixed or superseded, and quoting all of them
// would spend the prompt re-litigating rounds that are over.
func TestTheNewestMarkedCommentIsTheOneThatCounts(t *testing.T) {
	tk := ticket.Ticket{Comments: []ticket.Comment{
		{Body: "an unrelated note"},
		{Body: ReturnedMarker + "\n\nthe filter is case-sensitive"},
		{Body: "someone replying"},
		{Body: ReturnedMarker + "\n\nit still drops empty titles"},
	}}

	got := LatestMarked(tk, ReturnedMarker)
	if !strings.Contains(got, "still drops empty titles") {
		t.Errorf("the newest finding was not returned: %q", got)
	}
	if strings.Contains(got, "case-sensitive") {
		t.Errorf("a superseded finding came back: %q", got)
	}
}

// THE MARKER IS A HEADING, NOT CONTENT. Repeating it inside the prompt tells the
// agent nothing it is not already being told in plainer words.
func TestTheMarkerAndTheFooterAreStripped(t *testing.T) {
	tk := ticket.Ticket{Comments: []ticket.Comment{{
		Body: ReturnedMarker + "\n\nthe filter is case-sensitive\n\n" +
			"<sub>qwen · 10/20 tokens · 400ms</sub>\n",
	}}}

	got := LatestMarked(tk, ReturnedMarker)
	if strings.Contains(got, ReturnedMarker) {
		t.Errorf("the marker survived: %q", got)
	}
	if strings.Contains(got, "<sub>") || strings.Contains(got, "tokens") {
		t.Errorf("the footer survived; it is this department talking to itself: %q", got)
	}
	if got != "the filter is case-sensitive" {
		t.Errorf("LatestMarked = %q", got)
	}
}

// A TICKET THAT WAS NEVER SENT BACK CARRIES NOTHING, and must not produce an
// empty heading in the prompt.
func TestATicketWithNoSuchCommentReturnsNothing(t *testing.T) {
	tk := ticket.Ticket{Comments: []ticket.Comment{{Body: "just a note"}}}
	for _, marker := range []string{ReturnedMarker, SpecRepairMarker} {
		if got := LatestMarked(tk, marker); got != "" {
			t.Errorf("marker %q returned %q", marker, got)
		}
	}
	if got := LatestMarked(ticket.Ticket{}, ReturnedMarker); got != "" {
		t.Errorf("a ticket with no comments returned %q", got)
	}
}

// A COMMENT THAT IS ONLY ITS MARKER carries no finding, and an empty body must
// not produce a heading with nothing under it.
func TestACommentThatIsOnlyItsMarkerCarriesNothing(t *testing.T) {
	tk := ticket.Ticket{Comments: []ticket.Comment{{Body: ReturnedMarker + "\n\n   \n"}}}
	if got := LatestMarked(tk, ReturnedMarker); got != "" {
		t.Errorf("an empty finding returned %q", got)
	}
}

// THE TWO DIRECTIONS OF THE ROUND TRIP ARE SEPARATE. The reviewer's send-back
// and the developer's hand-back mean opposite things, and reading one with the
// other's marker is how the hand-back stayed write-only: the reason for it was
// posted, counted, and never rendered into a prompt.
func TestEachRoundTripReadsOnlyItsOwnMarker(t *testing.T) {
	tk := ticket.Ticket{Comments: []ticket.Comment{
		{Body: ReturnedMarker + "\n\nthe reviewer's finding"},
		{Body: SpecRepairMarker + "\n\nthe developer's hand-back"},
	}}

	if got := LatestMarked(tk, ReturnedMarker); !strings.Contains(got, "reviewer's finding") {
		t.Errorf("the reviewer's marker read %q", got)
	}
	if got := LatestMarked(tk, SpecRepairMarker); !strings.Contains(got, "developer's hand-back") {
		t.Errorf("the hand-back marker read %q", got)
	}
}
