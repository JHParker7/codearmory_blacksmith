package main

import (
	"context"
	"strings"
	"testing"
)

// A REQUEST WHOSE WORK IS ALL DONE MUST STOP SAYING IT IS WAITING.
//
// ColTracking is only ever a destination — the product manager's Success — and no
// stage names it as Ready, so nothing ever claimed a tracking ticket and a
// broken-down request stayed there for good. The intent was that the parent
// remain visible; the effect was that a request could never be finished, however
// much of it was.
//
// Reported from the window on r76 and correct: five tasks and eleven sections all
// done, the delivered code building with 37 tests passing, and the request still
// showing as waiting. Nothing anywhere said the job was complete.

func trackingDispatcher(api *CodeArmory) *Dispatcher {
	// The product manager is the stage whose Success is tracking, so it is the one
	// that sweeps it — exactly one, or seven dispatchers race for one ticket.
	return NewDispatcher(api, nil, &stubHandler{role: "pm-agent"}, DispatcherOpts{Host: "osiris"})
}

func addChild(t *testing.T, f *fakePlatform, id, parent, status string) {
	t.Helper()
	p := parent
	f.addTicket(t, Ticket{TicketID: id, ParentID: &p, Status: status, CreatedBy: "alice"})
}

func TestARequestClosesOnceEveryTaskIsDone(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "request0", Status: ColTracking, CreatedBy: "alice"})
	addChild(t, f, "task0001", "request0", ColDone)
	addChild(t, f, "task0002", "request0", ColDone)

	trackingDispatcher(api).closeFinishedRequests(context.Background())

	if got := f.get("request0").Status; got != ColDone {
		t.Fatalf("status = %q, want %q: every task is done and the request still says it is waiting", got, ColDone)
	}
	var explained bool
	for _, c := range f.get("request0").Comments {
		if strings.Contains(c.Body, "Request complete") {
			explained = true
		}
	}
	if !explained {
		t.Error("the request was closed with no comment saying why")
	}
}

// A BLOCKED OR UNFINISHED TASK MEANS THE REQUEST IS NOT FINISHED, and saying it
// is would hide exactly the case an operator needs to see.
func TestARequestStaysOpenWhileAnyTaskIsUnfinished(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "request0", Status: ColTracking, CreatedBy: "alice"})
	addChild(t, f, "task0001", "request0", ColDone)
	addChild(t, f, "task0002", "request0", ColBlocked)

	trackingDispatcher(api).closeFinishedRequests(context.Background())

	if got := f.get("request0").Status; got != ColTracking {
		t.Errorf("status = %q, want %q: one task is blocked, so the request is not complete", got, ColTracking)
	}
}

// A request with no children has not been broken down, so there is nothing to
// conclude from — closing it would report work that was never planned as done.
func TestARequestWithNoTasksIsLeftAlone(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "request0", Status: ColTracking, CreatedBy: "alice"})

	trackingDispatcher(api).closeFinishedRequests(context.Background())

	if got := f.get("request0").Status; got != ColTracking {
		t.Errorf("status = %q, want %q: a request with no tasks was closed anyway", got, ColTracking)
	}
}

// ONLY ONE STAGE SWEEPS THIS. Tracking is nobody's Ready column, so without this
// guard every dispatcher would poll it and race to move the same ticket.
func TestOnlyTheStageThatParksARequestClosesIt(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "request0", Status: ColTracking, CreatedBy: "alice"})
	addChild(t, f, "task0001", "request0", ColDone)

	other := NewDispatcher(api, nil, &stubHandler{role: "dev-agent"}, DispatcherOpts{Host: "osiris"})
	other.closeFinishedRequests(context.Background())

	if got := f.get("request0").Status; got != ColTracking {
		t.Errorf("the dev stage closed a request it does not own (status %q)", got)
	}
}
