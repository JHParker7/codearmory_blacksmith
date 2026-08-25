package record

import (
	"strings"

	"github.com/code-armory-app/blacksmith/internal/ticket"
)

// LatestMarked returns the newest comment carrying a marker, stripped of the
// marker itself and of the token/latency footer a stage appends.
//
// THE LAST ONE WINS, NOT THE FIRST. A ticket can be sent back more than once,
// and the finding that matters is the one from the round just finished:
// everything before it has either been fixed or superseded, and quoting all of
// them would spend the prompt re-litigating rounds that are over.
//
// THIS IS WHAT MAKES A ROUND TRIP CONVERGE. Both directions were write-only for
// a time — a comment was posted, a counter bounded the trips, and nothing ever
// rendered the BODY into a prompt. Measured on r68: a section panicked in its
// own fixture, the developer handed it back twice, and both times the author
// opened with "I'll write the unit tests for the status parameter filtering
// functionality". It had no idea it was a repair, rewrote all 215 lines from
// scratch and reproduced the identical bug, until the ceiling stopped it.
func LatestMarked(t ticket.Ticket, marker string) string {
	var latest string
	for _, c := range t.Comments {
		if strings.Contains(c.Body, marker) {
			latest = c.Body
		}
	}
	if latest == "" {
		return ""
	}

	// The marker is a heading, not content: repeating it inside the prompt tells
	// the agent nothing it is not already being told in plainer words.
	body := strings.ReplaceAll(latest, marker, "")

	// And the footer is this department talking to itself.
	if i := strings.LastIndex(body, "<sub>"); i >= 0 {
		body = body[:i]
	}
	return strings.TrimSpace(body)
}
