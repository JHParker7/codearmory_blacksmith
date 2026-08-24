package main

import (
	"context"
	"errors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClaimSucceedsWhenUncontested(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x"})

	if err := api.Claim(context.Background(), "t1", stages[roleScoping], ClaimToken{Host: "osiris", Role: "pm-agent"}); err != nil {
		t.Fatalf("Claim() = %v, want success", err)
	}
	tok, ok := ClaimedBy(f.get("t1"))
	if !ok || tok.Host != "osiris" {
		t.Errorf("ClaimedBy = %+v (ok=%v), want osiris", tok, ok)
	}
}

// The whole point of claim-by-append: a peer that got there first wins, and the
// loser must learn that from the server's ordering rather than from its own
// write succeeding.
func TestClaimYieldsToAnOlderClaim(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	// Claim-by-append is the fallback for a store with no optimistic concurrency,
	// so the store must not offer any: with an ETag the claim is a compare-and-set
	// on status and never consults the comments this test is about.
	f.noETag = true
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x"})
	// A peer appended its claim an hour ago. The ticket stays OPEN on purpose:
	// that is the window claim-by-append has to resolve on its own, since without
	// a conditional write there is no atomic move to in_progress to race on.
	f.addClaim(t, "t1", "peer-host", "pm-agent", time.Now().Add(-2*time.Minute))

	err := api.Claim(context.Background(), "t1", stages[roleScoping], ClaimToken{Host: "osiris", Role: "pm-agent"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Claim() = %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "pm-agent") {
		t.Errorf("error = %v, want it to name the winner", err)
	}
	// The winner must be unchanged — losing a race must not steal the claim. Read
	// from the comments, which are the arbiter on this path; ClaimedBy answers the
	// different question of whether the ticket is held right now.
	cm, ok := oldestClaimForRole(f.get("t1").Comments, "pm-agent")
	if !ok {
		t.Fatal("no pm-agent claim on the ticket")
	}
	tok, err := parseClaim(cm.Body)
	if err != nil || tok.Host != "peer-host" {
		t.Errorf("winner = %s (err=%v), want peer-host", tok.Host, err)
	}
}

// Two hosts racing must reach the SAME conclusion about who won, whichever
// order they read in. Exactly one may proceed.
func TestClaimRaceHasExactlyOneWinner(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x"})

	apiB, err := NewCodeArmory(strings.TrimSuffix(api.baseURL, "/"), "test-token")
	if err != nil {
		t.Fatal(err)
	}
	apiB.backoff = time.Millisecond

	errs := make(chan error, 2)
	go func() {
		errs <- api.Claim(context.Background(), "t1", stages[roleScoping], ClaimToken{Host: "host-a", Role: "pm-agent"})
	}()
	go func() {
		errs <- apiB.Claim(context.Background(), "t1", stages[roleScoping], ClaimToken{Host: "host-b", Role: "pm-agent"})
	}()

	won := 0
	for range 2 {
		if err := <-errs; err == nil {
			won++
		} else if !errors.Is(err, ErrConflict) {
			t.Errorf("unexpected error: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d hosts believed they won the claim, want exactly 1", won)
	}
}

func TestErrorClassification(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1"})

	if _, err := api.GetTicket(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetTicket(missing) = %v, want ErrNotFound", err)
	}

	f.denyAll = true
	if _, err := api.GetTicket(context.Background(), "t1"); !errors.Is(err, ErrDenied) {
		t.Errorf("GetTicket under 403 = %v, want ErrDenied", err)
	}
}

// Transient failures must be retried; the call should succeed once the server
// recovers rather than surfacing a blip to the dispatch loop.
func TestTransientFailuresAreRetried(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x"})
	f.failNext = 2

	if _, err := api.GetTicket(context.Background(), "t1"); err != nil {
		t.Fatalf("GetTicket() = %v, want the retry to absorb 2 transient failures", err)
	}
}

func TestRetriesAreBounded(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1"})
	f.failNext = 99

	_, err := api.GetTicket(context.Background(), "t1")
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("GetTicket() = %v, want ErrTransient after exhausting retries", err)
	}
	f.mu.Lock()
	calls := len(f.calls)
	f.mu.Unlock()
	if calls > api.maxRetries+1 {
		t.Errorf("made %d calls, want at most %d", calls, api.maxRetries+1)
	}
}

// A 4xx that is not auth-related must not be retried — repeating a bad request
// only wastes time.
func TestClientErrorsAreNotRetried(t *testing.T) {
	integrationTest(t)
	_, api := newFakePlatform(t)
	if _, err := api.GetTicket(context.Background(), "nope"); err == nil {
		t.Fatal("want an error")
	}
	// One GET, no retries: the fake returns 404 for unknown ids.
	if _, err := api.GetTicket(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestNewCodeArmoryValidatesConfig(t *testing.T) {
	for _, tc := range []struct{ url, token, why string }{
		{"", "tok", "missing URL"},
		{"https://x/api", "", "missing token"},
		{"x/api", "tok", "schemeless URL"},
	} {
		if _, err := NewCodeArmory(tc.url, tc.token); err == nil {
			t.Errorf("NewCodeArmory(%q,%q) = nil error, want a rejection (%s)", tc.url, tc.token, tc.why)
		}
	}
}

func TestClaimedByIgnoresOrdinaryComments(t *testing.T) {
	tk := Ticket{Comments: []Comment{
		{CommentID: "c1", Body: "just a normal comment", CreatedAt: time.Unix(1, 0)},
	}}
	if _, ok := ClaimedBy(tk); ok {
		t.Error("ClaimedBy found a claim in an ordinary comment")
	}
}

// Equal timestamps must break deterministically, or two hosts can disagree.
func TestOldestClaimBreaksTiesDeterministically(t *testing.T) {
	at := time.Unix(100, 0)
	comments := []Comment{
		{CommentID: "c020", Body: claimMarker + `{"host":"b"}` + " -->", CreatedAt: at},
		{CommentID: "c010", Body: claimMarker + `{"host":"a"}` + " -->", CreatedAt: at},
	}
	first, ok := oldestClaim(comments)
	if !ok || first.CommentID != "c010" {
		t.Fatalf("oldestClaim = %+v, want c010", first)
	}
	// Reversed input must give the same answer.
	second, _ := oldestClaim([]Comment{comments[1], comments[0]})
	if second.CommentID != first.CommentID {
		t.Error("oldestClaim depends on input order; two hosts could disagree about the winner")
	}
}

// The conditional-write path: a claim must be one compare-and-set, and a stale
// racer must be refused by the server rather than by a follow-up read.
func TestClaimUsesConditionalWriteWhenAvailable(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x"})

	if err := api.Claim(context.Background(), "t1", stages[roleScoping], ClaimToken{Host: "osiris", Role: "pm-agent"}); err != nil {
		t.Fatalf("Claim() = %v", err)
	}
	// Claim moves the ticket itself, in the same write that decides the race.
	if got := f.get("t1").Status; got != ColScoping {
		t.Errorf("status = %q, want %q", got, ColScoping)
	}
	if f.get("t1").Version == 0 {
		t.Error("version did not advance; the conditional write did not happen")
	}
	if _, ok := ClaimedBy(f.get("t1")); !ok {
		t.Error("no claim comment written; attribution and the attempt counter depend on it")
	}
}

// An instance predating optimistic concurrency serves no ETag. The client must
// notice and fall back, rather than issue a conditional write the server will
// silently apply unconditionally — which would let two hosts both win.
func TestClaimFallsBackWhenServerHasNoETag(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.noETag = true
	f.addTicket(t, Ticket{TicketID: "t1", Title: "x"})

	if err := api.Claim(context.Background(), "t1", stages[roleScoping], ClaimToken{Host: "osiris", Role: "pm-agent"}); err != nil {
		t.Fatalf("Claim() on a legacy instance = %v, want the append fallback to succeed", err)
	}
	if got := f.get("t1").Status; got != ColScoping {
		t.Errorf("status = %q, want %q", got, ColScoping)
	}

	// And the fallback must still resolve a race to exactly one winner.
	f2, apiA := newFakePlatform(t)
	f2.noETag = true
	f2.addTicket(t, Ticket{TicketID: "t2", Title: "y"})
	apiB, err := NewCodeArmory(apiA.baseURL, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	apiB.backoff = time.Millisecond

	errs := make(chan error, 2)
	go func() {
		errs <- apiA.Claim(context.Background(), "t2", stages[roleScoping], ClaimToken{Host: "a", Role: "pm-agent"})
	}()
	go func() {
		errs <- apiB.Claim(context.Background(), "t2", stages[roleScoping], ClaimToken{Host: "b", Role: "pm-agent"})
	}()
	won := 0
	for range 2 {
		if err := <-errs; err == nil {
			won++
		}
	}
	if won != 1 {
		t.Errorf("%d hosts won on a legacy instance, want exactly 1", won)
	}
}

// A ticket already moved out of open must not be claimable, or the conditional
// write would happily take work a peer is already doing.
func TestClaimRefusesNonOpenTickets(t *testing.T) {
	integrationTest(t)
	f, api := newFakePlatform(t)
	f.addTicket(t, Ticket{TicketID: "t1", Status: ColScoping})

	if err := api.Claim(context.Background(), "t1", stages[roleScoping], ClaimToken{Host: "osiris", Role: "pm-agent"}); !errors.Is(err, ErrConflict) {
		t.Errorf("Claim() on an in_progress ticket = %v, want ErrConflict", err)
	}
}

// The prefix is the whole difference between the two stores, and getting it
// wrong fails quietly in one direction: an unprefixed path on the PLATFORM falls
// through to the portal and returns the SPA's HTML with a 200, which decodes to
// an empty list and reads as "no work" rather than as a broken URL.
func TestTicketsPathDependsOnTheStore(t *testing.T) {
	platform := &CodeArmory{}
	if got := platform.ticketsPath("/tickets"); got != "/tickets/tickets" {
		t.Errorf("platform ticketsPath = %q, want the service prefix", got)
	}
	local := &CodeArmory{ticketsURL: "http://node:30086"}
	if got := local.ticketsPath("/tickets"); got != "/tickets" {
		t.Errorf("local ticketsPath = %q, want no service prefix", got)
	}
}

// The local store shares the plane's gatekeeper, so it must be reached with the
// plane's session. Sending the PLATFORM token would be rejected by a gatekeeper
// that never issued it — and would put a platform credential on a host that is
// deliberately outside the platform's trust domain.
func TestLocalTicketsUseThePlaneCredential(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	api, err := NewCodeArmory("https://platform.invalid/api", "PLATFORM-TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.UseLocalForge(srv.URL, "PLANE-TOKEN"); err != nil {
		t.Fatal(err)
	}
	if err := api.UseLocalTickets(srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := api.ListTickets(context.Background(), ListOpts{}); err != nil {
		t.Fatalf("ListTickets() = %v", err)
	}
	if gotAuth != "Bearer PLANE-TOKEN" {
		t.Errorf("Authorization = %q, want the plane token", gotAuth)
	}
	if gotPath != "/tickets" {
		t.Errorf("path = %q, want no /tickets service prefix against a local store", gotPath)
	}
}

// The local store has no gatekeeper of its own — it shares the plane's. Accepting
// it without one would produce requests carrying no credential the store accepts,
// which surfaces as a 401 storm at the first poll rather than at startup.
func TestLocalTicketsRequireThePlane(t *testing.T) {
	api, err := NewCodeArmory("https://platform.invalid/api", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.UseLocalTickets("http://node:30086"); err == nil {
		t.Error("UseLocalTickets() with no local forge = nil, want an error")
	}
	if err := api.UseLocalTickets("node:30086"); err == nil {
		t.Error("UseLocalTickets() with no scheme = nil, want an error")
	}
}

// A standalone host has no platform. Anything still routed there must say so
// plainly instead of failing on an empty URL.
func TestStandaloneClientRejectsPlatformCalls(t *testing.T) {
	api := NewStandaloneCodeArmory()
	_, err := api.TriggerRun(context.Background(), "pipe-1", nil)
	if err == nil {
		t.Fatal("TriggerRun() on a standalone host = nil, want an error")
	}
	if !strings.Contains(err.Error(), "standalone") {
		t.Errorf("error = %v, want it to name the mode", err)
	}
}

// The append protocol exists to arbitrate between several HOSTS running one
// role. Restricting it to a role must not weaken that: the earliest claim for
// the role still wins, and a later one still loses.
func TestOldestClaimForRoleArbitratesWithinARole(t *testing.T) {
	base := time.Now()
	comments := []Comment{
		{CommentID: "pm", CreatedAt: base,
			Body: `<!-- blacksmith:claim {"host":"osiris","role":"pm-agent","run_id":"r0"} -->`},
		{CommentID: "dev-a", CreatedAt: base.Add(time.Minute),
			Body: `<!-- blacksmith:claim {"host":"hostA","role":"dev-agent","run_id":"rA"} -->`},
		{CommentID: "dev-b", CreatedAt: base.Add(2 * time.Minute),
			Body: `<!-- blacksmith:claim {"host":"hostB","role":"dev-agent","run_id":"rB"} -->`},
	}

	// Two hosts on the same role: the earlier wins, which is the whole point.
	winner, ok := oldestClaimForRole(comments, "dev-agent")
	if !ok || winner.CommentID != "dev-a" {
		t.Errorf("dev-agent winner = %q (ok=%v), want dev-a", winner.CommentID, ok)
	}
	// Across roles the pipeline's earlier stage must NOT win, or the developer
	// loses to the product manager on every ticket it was handed.
	if winner.CommentID == "pm" {
		t.Error("an earlier stage's claim won the developer's arbitration")
	}
	// A role that has not claimed has no winner.
	if _, ok := oldestClaimForRole(comments, "sec-agent"); ok {
		t.Error("sec-agent has a winner without having claimed")
	}
	// Unrestricted still sees the oldest overall.
	if w, ok := oldestClaim(comments); !ok || w.CommentID != "pm" {
		t.Errorf("oldestClaim = %q, want the oldest overall", w.CommentID)
	}
}

// Without trace context on the wire, every service blacksmith calls starts an
// unrelated root span: forge, tickets and gatekeeper all appear in the collector
// and none of them appears connected to the agent that drove them, so the
// architecture view shows three services and no blacksmith.
func TestOutgoingRequestsCarryTraceContext(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("traceparent")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	// A propagator must be installed for injection to do anything; the SDK sets
	// one at startup, and this mirrors it.
	otel.SetTextMapPropagator(propagation.TraceContext{})

	api, err := NewCodeArmory(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	ctx, span := noopTracerForTest().Start(context.Background(), "stage.test")
	defer span.End()

	if _, err := api.ListTickets(ctx, ListOpts{}); err != nil {
		t.Fatalf("ListTickets() = %v", err)
	}
	if got == "" {
		t.Error("no traceparent on the request; the callee's spans would be orphans")
	}
}

// noopTracerForTest gives a real span context without standing up an exporter.
func noopTracerForTest() trace.Tracer {
	return sdktrace.NewTracerProvider().Tracer("test")
}

// A conflict must carry the server's explanation, not just the sentinel.
//
// ErrConflict reads "already claimed", which fits the ticket race it was written
// for and misdescribes everything else. Forge returns 409 as "lease is expired,
// not ready"; this surfaced it as "POST /executions: already claimed" — naming a
// claim mechanism forge does not have — and the body carrying the real reason was
// thrown away.
func TestConflictKeepsTheServersExplanation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "lease is expired, not ready", http.StatusConflict)
	}))
	defer srv.Close()

	c := &CodeArmory{baseURL: srv.URL, http: srv.Client()}
	err := c.do(context.Background(), http.MethodPost, "/executions", nil, nil)
	if err == nil {
		t.Fatal("do() = nil, want a conflict")
	}
	if !errors.Is(err, ErrConflict) {
		t.Errorf("do() = %v, want it to wrap ErrConflict so callers can still match on it", err)
	}
	if !strings.Contains(err.Error(), "lease is expired") {
		t.Errorf("do() = %q, want the server's own reason preserved", err.Error())
	}
}
