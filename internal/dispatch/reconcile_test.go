package dispatch

import (
	"context"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// claimedBy leaves a ticket held in the working column, the way a killed process
// would have left it.
func claimedBy(t *testing.T, b *board, st workflow.Stage, host string) *ticket.Ticket {
	t.Helper()
	tk := b.add(ticket.Ticket{Status: st.Ready})
	if err := b.Claim(context.Background(), tk.ID, st, record.Claim{Host: host, Role: workflow.RoleDev}); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return tk
}

// AN INTERRUPTED ATTEMPT IS NOT A SPENT ONE. The ceiling counts claims, a claim
// is written when work starts, and nothing closes it when the process is killed
// — so every restart to install a fix charges the in-flight ticket an attempt.
// Measured: six restarts in a morning put three claims on one ticket, which then
// sat unclaimable for eighteen minutes until the first aged out.
func TestReconcileReleasesThisHostsInterruptedWorkAndRefundsTheAttempt(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := claimedBy(t, b, st, "gpu-1")

	d := newDispatcher(t, b, &stubHandler{role: workflow.RoleDev}, Options{Host: "gpu-1"})
	if err := d.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := b.get(tk.ID)
	if got.Status != st.Ready {
		t.Errorf("the ticket is in %q, want its queue", got.Status)
	}
	// THE REFUND IS THE POINT. Releasing without it leaves the attempt spent, and
	// three restarts make the ticket permanently unclaimable.
	if n := record.Attempts(got, workflow.RoleDev); n != 0 {
		t.Errorf("the ticket still records %d attempts after an interrupted one was cleared", n)
	}
	var explained bool
	for _, c := range got.Comments {
		if strings.Contains(c.Body, "Attempt interrupted") {
			explained = true
		}
	}
	if !explained {
		t.Error("nothing on the ticket says why it went back; a silent move is a stall nobody can read")
	}
}

// SCOPED TO THIS HOST. Another host's in-flight work is indistinguishable from a
// stranded claim when viewed from here, so an unscoped sweep would steal live
// work from a peer.
func TestReconcileLeavesAnotherHostsWorkAlone(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := claimedBy(t, b, st, "gpu-2")

	d := newDispatcher(t, b, &stubHandler{role: workflow.RoleDev}, Options{Host: "gpu-1"})
	if err := d.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if got := b.get(tk.ID); got.Status != st.Working {
		t.Errorf("a peer's live work was released to %q", got.Status)
	}
}

// WORK THIS PROCESS IS STILL DOING IS NOT STRANDED. Releasing it would put a
// second agent on a ticket this one is holding.
func TestReconcileLeavesLiveWorkAlone(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := claimedBy(t, b, st, "gpu-1")

	d := newDispatcher(t, b, &stubHandler{role: workflow.RoleDev}, Options{Host: "gpu-1"})
	d.mu.Lock()
	d.inFlight[tk.ID] = true
	d.mu.Unlock()

	if err := d.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := b.get(tk.ID); got.Status != st.Working {
		t.Errorf("work this process is still doing was released to %q", got.Status)
	}
}

// THE RE-READ IS LOAD-BEARING. Whose claim a ticket carries lives in a COMMENT
// and a listing has none, so judged from the listing alone no ticket ever looks
// like this host's — nothing is released and stranded claims accumulate
// silently, which is the exact failure Reconcile exists to prevent.
func TestReconcileReadsTheTicketRatherThanTheListing(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := claimedBy(t, b, st, "gpu-1")

	// If the ticket cannot be re-read, nothing can be concluded about it and it is
	// left held rather than released on a guess.
	b.failGet[tk.ID] = true

	d := newDispatcher(t, b, &stubHandler{role: workflow.RoleDev}, Options{Host: "gpu-1"})
	if err := d.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := b.get(tk.ID); got.Status != st.Working {
		t.Errorf("a ticket that could not be read was released to %q on a guess", got.Status)
	}
}

// A ticket in the working column with no claim on it at all belongs to nobody
// this sweep can speak for, and is left where it is.
func TestReconcileIgnoresAnUnclaimedTicket(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := b.add(ticket.Ticket{Status: st.Working})

	d := newDispatcher(t, b, &stubHandler{role: workflow.RoleDev}, Options{Host: "gpu-1"})
	if err := d.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := b.get(tk.ID); got.Status != st.Working {
		t.Errorf("a ticket with no claim was moved to %q", got.Status)
	}
}

// A RELEASED TICKET IS IMMEDIATELY CLAIMABLE AGAIN — the refund is only worth
// anything if the next poll can actually take the work.
func TestAReconciledTicketIsWorkedOnTheNextPoll(t *testing.T) {
	b := newBoard()
	st := devStage(t)
	tk := claimedBy(t, b, st, "gpu-1")

	h := &stubHandler{role: workflow.RoleDev}
	d := newDispatcher(t, b, h, Options{Host: "gpu-1", MaxAttempts: 1})
	if err := d.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	drainOnce(t, d, 1)

	if got := h.worked(); len(got) != 1 || got[0] != tk.ID {
		t.Errorf("worked %v, want the reconciled ticket — with one attempt configured, "+
			"the refund is the only thing that makes it claimable", got)
	}
}
