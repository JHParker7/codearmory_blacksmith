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

func TestListFiltersServerSide(t *testing.T) {
	f, store := newFakeStore(t)
	board := "b-1"
	f.add(ticket.Ticket{Status: workflow.ColReadyForDev, BoardID: &board})
	f.add(ticket.Ticket{Status: workflow.ColInDev, BoardID: &board})
	f.add(ticket.Ticket{Status: workflow.ColReadyForDev, BoardID: ticket.Ptr("b-2")})

	got, err := store.List(context.Background(), ticket.ListOpts{
		BoardID: board, Status: workflow.ColReadyForDev,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List returned %d tickets, want the one in the queue on this board", len(got))
	}
	// The filter must reach the STORE, not be applied here: a stage that fetched
	// the whole board and discarded most of it is what the column design avoids.
	var sawQuery bool
	for _, r := range f.seen() {
		if strings.Contains(r, "/tickets") {
			sawQuery = true
		}
	}
	if !sawQuery {
		t.Error("no listing request reached the store")
	}
}

// A LISTING DOES NOT CARRY COMMENTS, and a stage that reads claims off one
// silently sees none — which is how a predicate inverted in the shape this
// replaces: a stage requiring a marker selected nothing forever.
func TestOnlyAFetchCarriesTheComments(t *testing.T) {
	f, store := newFakeStore(t)
	tk := f.add(ticket.Ticket{Status: workflow.ColReadyForDev})
	f.appendComment(tk.ID, "agent", "a comment")

	listed, err := store.List(context.Background(), ticket.ListOpts{Status: workflow.ColReadyForDev})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || len(listed[0].Comments) != 0 {
		t.Errorf("a listing carried comments; selection built on that would read a different board")
	}

	fetched, err := store.Get(context.Background(), tk.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(fetched.Comments) != 1 {
		t.Errorf("a fetch returned %d comments, want 1", len(fetched.Comments))
	}
}

// THE PREFIX TRAP. Conductor routes by service name, so a path without /tickets
// does not 404 against the platform — it returns the portal's HTML with a 200,
// which decodes to an empty list and reads as "no work today".
func TestARoutedStoreCarriesTheServicePrefix(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	routed, err := Routed(srv.URL, transport.Static("tok"))
	if err != nil {
		t.Fatalf("Routed: %v", err)
	}
	if _, err := routed.List(context.Background(), ticket.ListOpts{}); err != nil {
		t.Fatalf("List: %v", err)
	}
	// Exactly, not by prefix: the resource is itself called /tickets, so "starts
	// with /tickets" is true of the unprefixed path too and would pass either way.
	if len(paths) != 1 || paths[0] != servicePrefix+"/tickets" {
		t.Fatalf("a routed store requested %q, want %q", paths, servicePrefix+"/tickets")
	}

	local, err := Local(srv.URL, transport.Static("tok"))
	if err != nil {
		t.Fatalf("Local: %v", err)
	}
	if _, err := local.List(context.Background(), ticket.ListOpts{}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if paths[1] != "/tickets" {
		t.Errorf("a plane store requested %q, want %q; nothing routes by service name there", paths[1], "/tickets")
	}
}

// The prefix is fixed by the constructor, so the wrong one cannot be selected
// per call — this pins that a routed store reaching a plane-shaped server does
// not silently succeed with an empty list.
func TestTheWrongPrefixIsNotSilentlyAnEmptyBacklog(t *testing.T) {
	f, _ := newFakeStore(t)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	local, err := Local(srv.URL, transport.Static("tok"))
	if err != nil {
		t.Fatalf("Local: %v", err)
	}
	_, err = local.List(context.Background(), ticket.ListOpts{})
	if err == nil {
		t.Fatal("a plane store reached the portal and reported an empty backlog")
	}
}

func TestBothConstructorsRejectAnUnusableURL(t *testing.T) {
	for _, build := range map[string]func(string, transport.Credential) (*Store, error){
		"routed": Routed, "local": Local,
	} {
		for _, raw := range []string{"", "   ", "host.example", "ftp://host"} {
			if _, err := build(raw, transport.Static("tok")); err == nil {
				t.Errorf("a store was built for %q", raw)
			}
		}
	}
}

// An unset field is UNCHANGED, and that is what makes a partial update safe: a
// stage recording a verdict must not blank the description it did not touch.
func TestUpdateLeavesUnsetFieldsAlone(t *testing.T) {
	f, store := newFakeStore(t)
	tk := f.add(ticket.Ticket{
		Title: "Build the store", Description: "the plan", Status: workflow.ColReadyForDev, Priority: "high",
	})

	if _, err := store.Update(context.Background(), tk.ID, ticket.Update{Status: workflow.ColInDev}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := f.get(tk.ID)
	if got.Status != workflow.ColInDev {
		t.Errorf("status = %q", got.Status)
	}
	if got.Title != "Build the store" || got.Description != "the plan" || got.Priority != "high" {
		t.Errorf("an unset field was overwritten: %+v", got)
	}
}

// MOVING A TICKET ON CLEARS THE ASSIGNEE. One that moves still carrying the last
// agent's name reads, on the board, as though that agent is still working it.
func TestMoveToHandsTheTicketBack(t *testing.T) {
	f, store := newFakeStore(t)
	tk := f.add(ticket.Ticket{Status: workflow.ColInDev, AssigneeID: ticket.Ptr("dev-agent@gpu-1")})

	if err := store.MoveTo(context.Background(), tk.ID, workflow.ColReadyForReview); err != nil {
		t.Fatalf("MoveTo: %v", err)
	}
	got, _ := f.get(tk.ID)
	if got.Status != workflow.ColReadyForReview {
		t.Errorf("status = %q", got.Status)
	}
	if got.Board() == "" && got.AssigneeID != nil {
		t.Errorf("the ticket still names %q; the next stage's claim would be the second write, not the first", *got.AssigneeID)
	}
}

// A TICKET OPENS IN THE COLUMN ITS CREATOR CHOSE. Sending an empty status would
// open it in a column no stage polls, and a child scoped into the inbox would be
// scoped again.
func TestCreateOpensTheTicketWhereItWasAsked(t *testing.T) {
	f, store := newFakeStore(t)
	parent := f.add(ticket.Ticket{Status: workflow.ColTracking})

	got, err := store.Create(context.Background(), ticket.Ticket{
		Title: "Section one", Description: "write the tests",
		Status: workflow.ColReadyForSpec, Priority: "medium",
		ParentID: &parent.ID, BoardID: ticket.Ptr("b-1"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.Status != workflow.ColReadyForSpec {
		t.Errorf("the ticket opened in %q", got.Status)
	}
	// The parent is what keeps a child findable from the request that produced it.
	if got.Parent() != parent.ID {
		t.Errorf("parent = %q, want %q", got.Parent(), parent.ID)
	}
	if got.Board() != "b-1" {
		t.Errorf("board = %q", got.Board())
	}
}

func TestCreateBoardRefusesAResponseWithNoID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	store, _ := Routed(srv.URL, transport.Static("tok"))

	if _, err := store.CreateBoard(context.Background(), "a project"); err == nil {
		t.Error("a board with no id was accepted; the project would point at nothing")
	}
}

// EnsureColumns is ADDITIVE. A startup path that removed columns it did not
// recognise would be a data-loss bug wearing the clothes of a reconciler.
func TestEnsureColumnsCreatesOnlyWhatIsMissing(t *testing.T) {
	var created []fieldDef
	existing := []fieldDef{
		{Kind: fieldKindStatus, Value: workflow.ColInbox},
		{Kind: fieldKindStatus, Value: "a column somebody added"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, existing)
			return
		}
		var f fieldDef
		decodeJSON(r, &f)
		created = append(created, f)
	}))
	t.Cleanup(srv.Close)
	store, _ := Routed(srv.URL, transport.Static("tok"))

	if err := store.EnsureColumns(context.Background(), "b-1"); err != nil {
		t.Fatalf("EnsureColumns: %v", err)
	}

	want := len(workflow.Columns()) - 1 // the inbox already exists
	if len(created) != want {
		t.Errorf("created %d columns, want %d", len(created), want)
	}
	for _, f := range created {
		if f.Value == workflow.ColInbox {
			t.Error("a column that already existed was created again")
		}
		if f.Value == "a column somebody added" {
			t.Error("a column this department does not know about was touched")
		}
		if f.BoardID != "b-1" || f.Kind != fieldKindStatus {
			t.Errorf("created %+v", f)
		}
	}
}

// A board id of "" is not an error: a project without one is a project whose
// tickets live wherever the store puts them, and provisioning nothing is right.
func TestEnsureColumnsDoesNothingWithoutABoard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a request was issued for a board that was not named")
	}))
	t.Cleanup(srv.Close)
	store, _ := Routed(srv.URL, transport.Static("tok"))

	if err := store.EnsureColumns(context.Background(), ""); err != nil {
		t.Errorf("EnsureColumns: %v", err)
	}
}
