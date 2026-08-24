package record

import (
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/ticket"
)

// AN UNREADABLE CLAIM MUST NOT HIDE A REAL HOLDER.
//
// A body carrying the marker but not decoding names nobody. Letting one win the
// "who holds this" question means a ticket whose column says it is held reports
// no holder at all — and the window then shows it as free while an agent is
// working it, which is the one thing the column was introduced to stop.
func TestAnUnreadableClaimDoesNotHideTheHolder(t *testing.T) {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	tk := ticket.Ticket{Comments: []ticket.Comment{
		{ID: "c-1", Body: ClaimMarker + "garbage -->", CreatedAt: at},
		claimComment("c-2", "dev-agent", "gpu-1", at.Add(time.Minute)),
	}}

	got, ok := HeldBy(tk, true)
	if !ok {
		t.Fatal("HeldBy found no holder; an unreadable claim hid a real one")
	}
	if got.Host != "gpu-1" || got.Role != "dev-agent" {
		t.Errorf("HeldBy = %+v, want the readable claim", got)
	}
}

// With nothing readable at all there is genuinely no holder to name, and saying
// so is right — the column still says the ticket is held, and inventing a name
// for it would be worse than admitting the record is damaged.
func TestNoReadableClaimMeansNoNamedHolder(t *testing.T) {
	tk := ticket.Ticket{Comments: []ticket.Comment{
		{ID: "c-1", Body: ClaimMarker + "garbage -->"},
	}}
	if _, ok := HeldBy(tk, true); ok {
		t.Error("HeldBy named a holder from a claim that does not decode")
	}
}

// The same rule in the arbitration: an unreadable claim cannot be shown to
// belong to a role, so it takes part in neither question.
func TestOldestSkipsWhatItCannotRead(t *testing.T) {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	comments := []ticket.Comment{
		{ID: "c-1", Body: ClaimMarker + "not json -->", CreatedAt: at},
		claimComment("c-2", "dev-agent", "gpu-1", at.Add(time.Minute)),
	}
	got, ok := Oldest(comments)
	if !ok {
		t.Fatal("Oldest found nothing among a readable claim and an unreadable one")
	}
	if got.ID != "c-2" {
		t.Errorf("Oldest picked %q, want the readable claim", got.ID)
	}
}
