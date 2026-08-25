package platform

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/transport"
)

// fakeStore is an in-memory ticket store served over real HTTP.
//
// A SERVER RATHER THAN A STUBBED CLIENT, deliberately. What is being tested here
// is a race protocol whose correctness lives in the interaction — the ETag on
// the read, the If-Match on the write, the ordering of appended comments — and a
// fake that answers method calls cannot get any of that wrong, so it cannot
// catch it being got wrong either.
type fakeStore struct {
	mu sync.Mutex

	tickets map[string]*ticket.Ticket
	seq     int

	// versioned reports whether this store supports conditional writes. Turning
	// it off is how the append fallback gets exercised: that path is chosen by
	// the ABSENCE of an ETag, which no amount of client-side testing reaches.
	versioned bool

	// now is the clock the store stamps comments with, so ordering is decided by
	// the test rather than by how fast the machine is.
	now time.Time

	// beforeComment runs before a comment is appended, and afterRead once a read
	// has been served. They are how a test puts another host's win at exactly the
	// wrong moment — afterRead in particular is the only way to reach the
	// conditional write's 412, since a competitor that wins BEFORE the read is
	// caught by the column check instead and the version is never tested.
	beforeComment func(id string)
	afterRead     func(id string)

	requests []string
}

func newFakeStore(t *testing.T) (*fakeStore, *Store) {
	t.Helper()
	f := &fakeStore{
		tickets:   map[string]*ticket.Ticket{},
		versioned: true,
		now:       time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	// Routed, so the /tickets prefix is exercised: an unprefixed path against the
	// platform returns the portal's HTML with a 200, which reads as an empty
	// backlog rather than as a misconfiguration.
	store, err := Routed(srv.URL, transport.Static("tok"))
	if err != nil {
		t.Fatalf("Routed: %v", err)
	}
	store.http.Backoff = time.Millisecond

	// THE STORE JUDGES STALENESS AGAINST THIS FAKE'S CLOCK, not the real one.
	// Comments here are stamped at a fixed date, so against time.Now() every
	// claim — including the one just written — reads as hours old and is pruned
	// as abandoned, leaving the claimant unable to find its own comment.
	store.now = f.clock
	return f, store
}

func (f *fakeStore) add(t ticket.Ticket) *ticket.Ticket {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t.ID == "" {
		f.seq++
		t.ID = fmt.Sprintf("t-%d", f.seq)
	}
	t.Version = 1
	f.tickets[t.ID] = &t
	return &t
}

// appendComment writes a comment the way the store does: a server-assigned id
// clock reads the fake's time under its lock. appendComment advances it.
func (f *fakeStore) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// and a server-assigned timestamp, in arrival order.
func (f *fakeStore) appendComment(id, author, body string) (ticket.Comment, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tickets[id]
	if !ok {
		return ticket.Comment{}, false
	}
	f.seq++
	f.now = f.now.Add(time.Second)
	c := ticket.Comment{
		ID: fmt.Sprintf("c-%d", f.seq), TicketID: id,
		AuthorID: author, Body: body, CreatedAt: f.now,
	}
	t.Comments = append(t.Comments, c)
	t.Version++
	return c, true
}

func (f *fakeStore) get(id string) (ticket.Ticket, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tickets[id]
	if !ok {
		return ticket.Ticket{}, false
	}
	return *t, true
}

func (f *fakeStore) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.requests))
	copy(out, f.requests)
	return out
}

func (f *fakeStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.mu.Unlock()

	path, ok := strings.CutPrefix(r.URL.Path, servicePrefix)
	if !ok {
		// THE TRAP, MODELLED. Conductor routes by service name, so an unprefixed
		// path does not 404 — it falls through to the portal and answers 200 with
		// the web application's HTML.
		w.Write([]byte("<!doctype html><html>the portal</html>"))
		return
	}

	switch {
	case path == "/tickets" && r.Method == http.MethodGet:
		f.list(w, r)
	case path == "/tickets" && r.Method == http.MethodPost:
		f.create(w, r)
	case strings.HasSuffix(path, "/comments") && r.Method == http.MethodPost:
		f.comment(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/tickets/"), "/comments"))
	case strings.HasSuffix(path, "/dependencies") && r.Method == http.MethodPost:
		w.WriteHeader(http.StatusNoContent)
	case path == "/boards" && r.Method == http.MethodPost:
		json.NewEncoder(w).Encode(map[string]string{"board_id": "b-1"})
	case path == "/field-defs" && r.Method == http.MethodGet:
		json.NewEncoder(w).Encode([]fieldDef{})
	case path == "/field-defs" && r.Method == http.MethodPost:
		w.WriteHeader(http.StatusCreated)
	case strings.HasPrefix(path, "/tickets/") && r.Method == http.MethodGet:
		f.read(w, strings.TrimPrefix(path, "/tickets/"))
	case strings.HasPrefix(path, "/tickets/") && r.Method == http.MethodPut:
		f.update(w, r, strings.TrimPrefix(path, "/tickets/"))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeStore) list(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	out := []ticket.Ticket{}
	for _, t := range f.tickets {
		if s := q.Get("status"); s != "" && t.Status != s {
			continue
		}
		if b := q.Get("board_id"); b != "" && t.Board() != b {
			continue
		}
		if p := q.Get("priority"); p != "" && t.Priority != p {
			continue
		}
		// A LISTING DOES NOT CARRY COMMENTS. Modelled, because a stage that reads
		// claims off a listing silently sees none — which inverted a predicate in
		// the shape this replaces.
		shallow := *t
		shallow.Comments = nil
		out = append(out, shallow)
	}
	json.NewEncoder(w).Encode(out)
}

func (f *fakeStore) read(w http.ResponseWriter, id string) {
	t, ok := f.get(id)
	if !ok {
		http.NotFound(w, nil)
		return
	}
	if f.versioned {
		w.Header().Set("ETag", strconv.FormatInt(t.Version, 10))
	}
	json.NewEncoder(w).Encode(t)
	if f.afterRead != nil {
		f.afterRead(id)
	}
}

func (f *fakeStore) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	json.NewDecoder(r.Body).Decode(&req)
	t := f.add(ticket.Ticket{
		Title: req.Title, Description: req.Description,
		Status: req.Status, Priority: req.Priority,
		ParentID: ticket.Ptr(req.ParentID), BoardID: ticket.Ptr(req.BoardID),
	})
	json.NewEncoder(w).Encode(t)
}

func (f *fakeStore) comment(w http.ResponseWriter, r *http.Request, id string) {
	if f.beforeComment != nil {
		f.beforeComment(id)
	}
	var body struct {
		Body string `json:"body"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	c, ok := f.appendComment(id, "agent", body.Body)
	if !ok {
		http.NotFound(w, r)
		return
	}
	json.NewEncoder(w).Encode(c)
}

func (f *fakeStore) update(w http.ResponseWriter, r *http.Request, id string) {
	var up ticket.Update
	json.NewDecoder(r.Body).Decode(&up)

	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tickets[id]
	if !ok {
		http.NotFound(w, r)
		return
	}

	// THE VERSION IS IN THE WHERE CLAUSE. This is the whole of the compare-and-set:
	// a stale token loses, and losing is a 412 rather than an error.
	if want := r.Header.Get("If-Match"); want != "" && want != strconv.FormatInt(t.Version, 10) {
		http.Error(w, "the ticket has moved on", http.StatusPreconditionFailed)
		return
	}

	if up.Status != "" {
		t.Status = up.Status
	}
	if up.Title != "" {
		t.Title = up.Title
	}
	if up.Description != "" {
		t.Description = up.Description
	}
	if up.Priority != "" {
		t.Priority = up.Priority
	}
	if up.AssigneeID != nil {
		t.AssigneeID = ticket.Ptr(*up.AssigneeID)
	}
	t.Version++
	json.NewEncoder(w).Encode(t)
}
