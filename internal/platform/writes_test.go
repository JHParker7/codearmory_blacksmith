package platform

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/transport"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// A dependency is what makes a stage WAIT, so a silent failure here produces a
// ticket that starts against a prerequisite that does not exist yet.
func TestAddDependencyNamesBothTicketsWhenItFails(t *testing.T) {
	f, store := newFakeStore(t)
	a := f.add(ticket.Ticket{Status: workflow.ColReadyForDev})
	b := f.add(ticket.Ticket{Status: workflow.ColReadyForDev})

	if err := store.AddDependency(context.Background(), a.ID, b.ID); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}

	// The message must name WHICH dependency could not be recorded — a stage
	// blocked on an unknown prerequisite is a stall nobody can act on.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such ticket", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	broken, _ := Routed(srv.URL, transport.Static("tok"))

	err := broken.AddDependency(context.Background(), "t-1", "t-2")
	if err == nil {
		t.Fatal("a dependency against a missing ticket was accepted")
	}
	if !strings.Contains(err.Error(), "t-1") || !strings.Contains(err.Error(), "t-2") {
		t.Errorf("err = %v, want both tickets named", err)
	}
}

// NOTHING IN THE AGENT PATH DELETES A TICKET, and nothing should: an agent that
// can delete tickets can erase the record of what it did. This exists for the
// end-to-end suite, which cleans up after itself.
func TestDeleteRemovesATicket(t *testing.T) {
	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	store, _ := Routed(srv.URL, transport.Static("tok"))

	if err := store.Delete(context.Background(), "t-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if method != http.MethodDelete || path != servicePrefix+"/tickets/t-1" {
		t.Errorf("issued %s %s", method, path)
	}
}

// A ticket id with a slash in it must not be able to address a different
// resource than the one named.
func TestATicketIDIsEscapedIntoThePath(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.EscapedPath()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	store, _ := Routed(srv.URL, transport.Static("tok"))

	if err := store.Delete(context.Background(), "t-1/../../boards/b-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if strings.Contains(path, "/boards/") {
		t.Errorf("an identifier walked out of its path segment: %q", path)
	}
}

// A comment is the audit trail and, for a claim, the attempt counter. It must
// come back with the store's own id and ordering rather than anything invented
// here — the append protocol arbitrates on exactly those.
func TestAddCommentReturnsTheStoresOwnRecord(t *testing.T) {
	f, store := newFakeStore(t)
	tk := f.add(ticket.Ticket{Status: workflow.ColReadyForDev})

	got, err := store.AddComment(context.Background(), tk.ID, "a note")
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if got.ID == "" {
		t.Error("the comment came back with no id; the append protocol arbitrates on it")
	}
	if got.CreatedAt.IsZero() {
		t.Error("the comment came back with no timestamp; ordering depends on it")
	}
	if got.Body != "a note" {
		t.Errorf("body = %q", got.Body)
	}
}

// A failure to move a ticket must name both the ticket and where it was going,
// or a stall reads as a stage that simply stopped.
func TestMoveToNamesTheTicketAndTheColumnOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	store, _ := Routed(srv.URL, transport.Static("tok"))

	err := store.MoveTo(context.Background(), "t-1", workflow.ColReadyForReview)
	if err == nil {
		t.Fatal("a move to a missing ticket succeeded")
	}
	if !strings.Contains(err.Error(), "t-1") || !strings.Contains(err.Error(), workflow.ColReadyForReview) {
		t.Errorf("err = %v, want the ticket and the destination named", err)
	}
}

// A failure listing the board's columns must not be reported as a failure to
// create one: the two send a reader to different places.
func TestEnsureColumnsSaysWhichHalfFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no grant", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	store, _ := Routed(srv.URL, transport.Static("tok"))

	err := store.EnsureColumns(context.Background(), "b-1")
	if err == nil {
		t.Fatal("a denied listing was treated as an empty board")
	}
	if !strings.Contains(err.Error(), "list columns") {
		t.Errorf("err = %v, want it to say the listing failed", err)
	}
}
