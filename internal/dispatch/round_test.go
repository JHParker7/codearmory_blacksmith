package dispatch

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

func closesOn(t *testing.T, b *board, id string) int {
	t.Helper()
	n := 0
	for _, cm := range b.get(id).Comments {
		if strings.HasPrefix(cm.Body, record.ClaimClosedMarker) {
			n++
		}
	}
	return n
}

// THE WIRING HALF OF THE STALL. record knows how to end a claim round and the
// store knows how to arbitrate on one, but neither matters unless the dispatcher
// actually writes the close — and until it did, a ticket that failed once was
// never attempted again: it sat in its queue collecting a claim comment per poll
// with nothing on the board to say why.
//
// The fake board here does NOT arbitrate — it takes the conditional-write path —
// which is exactly why every Go test passed while the live pipeline stalled. So
// this asserts the WRITE, and platform's fallback tests assert the arbitration.
func TestAFinishedAttemptClosesItsClaimRound(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status workflow.Outcome
	}{
		{"a failure that will be retried", workflow.OutcomeFailed},
		{"a success that hands the ticket on", workflow.OutcomeSuccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBoard()
			st := devStage(t)
			tk := b.add(ticket.Ticket{Status: st.Ready})

			h := &stubHandler{role: workflow.RoleDev, status: tc.status, detail: "no"}
			d := newDispatcher(t, b, h, Options{Poll: 5 * time.Millisecond})

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() { _ = d.Run(ctx) }()

			eventually(t, "the ticket to leave the working column", func() bool {
				return b.get(tk.ID).Status != st.Working
			})
			eventually(t, "the claim round to be closed", func() bool {
				return closesOn(t, b, tk.ID) > 0
			})

			b.mu.Lock()
			seen := append([]bool(nil), b.closedAtMove...)
			b.mu.Unlock()
			if len(seen) == 0 {
				t.Fatal("the ticket never moved")
			}
			if !seen[0] {
				t.Fatal("the ticket left the working column with its claim round still " +
					"open; the next attempt will lose arbitration to this dead claim")
			}
		})
	}
}

// A ticket released before any work — the re-read after the claim failed — has
// still had a claim written for it, so its round must end too.
func TestAReleasedTicketClosesItsClaimRound(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Ready})

	// Wants says no only on the re-read, which is the release path: the listing
	// has no comments and the ticket read back after the claim has one.
	h := &stubHandler{role: workflow.RoleDev, wants: func(t ticket.Ticket) bool {
		return len(t.Comments) == 0
	}}
	d := newDispatcher(t, b, h, Options{Poll: 5 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Run(ctx) }()

	eventually(t, "the claim round to be closed on release", func() bool {
		return closesOn(t, b, tk.ID) > 0
	})
	if worked := h.worked(); len(worked) != 0 {
		t.Fatalf("the handler ran after refusing the ticket: %v", worked)
	}
}
