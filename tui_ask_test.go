package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// THE REFINER MAY NOT ADD REQUIREMENTS.
//
// Everything downstream treats the request as the authority on what was asked
// for: the product manager draws acceptance criteria from it, the specification
// author is told anything the request does not state is NOT a requirement, and
// the developer must satisfy every test written from it. A refiner that helpfully
// adds "and it should be paginated" creates work nobody asked for and which no
// one can tell apart from work someone did.

func askServer(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + strconv.Quote(reply) + `},"finish_reason":"stop"}]}`))
	}))
}

func askGateway(url string) *Gateway {
	return NewGateway(Config{Classes: map[Class]ClassConfig{
		ClassLarge: {Endpoint: url, Model: "m", Slots: 1, QueueDepth: 1},
	}})
}

func TestARefinedRequestKeepsWhatWasTyped(t *testing.T) {
	srv := askServer(t, `{"title":"Add a health endpoint","request":"Expose an endpoint that reports whether the service is healthy."}`)
	defer srv.Close()

	title, body := refineRequest(context.Background(), askGateway(srv.URL), ClassLarge, "add a health endpoint")

	if title != "Add a health endpoint" {
		t.Errorf("title = %q", title)
	}
	// The refiner is the only stage here nobody reviews. If it has quietly changed
	// the meaning, the original has to be on the ticket to be read against it.
	if !strings.Contains(body, "Asked for as: add a health endpoint") {
		t.Errorf("the typed line is not on the ticket: %q", body)
	}
}

// THE INPUT IS NEVER LOST. A request the person has already written is worth more
// than a better-shaped one they have to type again.
func TestAFailedRefinementFilesTheLineAsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	title, _ := refineRequest(context.Background(), askGateway(srv.URL), ClassLarge, "add a health endpoint")
	if title != "add a health endpoint" {
		t.Errorf("title = %q, want the line exactly as typed", title)
	}
}

func TestNoModelStillFilesTheLine(t *testing.T) {
	title, body := refineRequest(context.Background(), nil, ClassLarge, "add a health endpoint")
	if title != "add a health endpoint" || body != "" {
		t.Errorf("title = %q body = %q; a window with no model must still file what was typed", title, body)
	}
}

func TestAnEmptyTitleFallsBackRatherThanFilingNothing(t *testing.T) {
	srv := askServer(t, `{"title":"   ","request":"something"}`)
	defer srv.Close()

	title, _ := refineRequest(context.Background(), askGateway(srv.URL), ClassLarge, "add a health endpoint")
	if title != "add a health endpoint" {
		t.Errorf("title = %q; a blank refined title must not become the ticket", title)
	}
}

// The prompt has to say the one thing that matters, because this is the only
// stage that writes the request everything else is measured against.
func TestTheAskPromptForbidsInventingRequirements(t *testing.T) {
	for _, want := range []string{"RESTATE, DO NOT DESIGN", "Do NOT add requirements"} {
		if !strings.Contains(askPrompt, want) {
			t.Errorf("the refiner is not told %q", want)
		}
	}
	if _, err := json.Marshal(askSchema()); err != nil {
		t.Fatalf("schema does not marshal: %v", err)
	}
}

// A BLOCKED TICKET IS NOT BEING WORKED, so its clock stops with it.
//
// Blocked means the department has given up and is waiting for a person, so
// nothing is being spent on it — and a counter that keeps climbing says the
// opposite, exactly when someone is working out how long the board has been stuck
// rather than busy. On r79 five tasks blocked at once and every one went on
// reporting time as though it were still being worked.
func TestABlockedTicketStopsCounting(t *testing.T) {
	created := time.Now().Add(-6 * time.Hour)
	tk := Ticket{
		TicketID:  "t1000000",
		Status:    ColBlocked,
		CreatedAt: created,
		UpdatedAt: created.Add(3*time.Minute + 20*time.Second),
	}

	d, final := ticketRuntime(tk)
	if !final {
		t.Error("a blocked ticket keeps counting; it is not being worked")
	}
	if got := runtimeText(d); got != "3m20s" {
		t.Errorf("runtime = %q, want %q — it is reporting how long it has been stuck, not how far it got", got, "3m20s")
	}
}

// UNFINISHED IS NOT FINISHED, whatever column it is in. Whether the clock TICKS
// is a separate question — a queued ticket reports no runtime at all, see
// TestAQueuedTicketShowsNoRuntime — but none of these may be reported as final,
// or the row would freeze on a number that is not the answer to anything.
//
// This test used to assert that a waiting ticket kept counting, on the reasoning
// that the wait is real. It is real and it is not work: on r85 four queued
// tickets showed fifteen minutes each while one dev agent worked, and the plain
// reading was that the run had slowed down.
func TestAnUnfinishedTicketIsNeverReportedAsFinal(t *testing.T) {
	for _, status := range []string{ColInbox, ColReadyForDev, ColInDev, ColTracking} {
		tk := Ticket{TicketID: "t1000000", Status: status, CreatedAt: time.Now().Add(-90 * time.Second)}
		if _, final := ticketRuntime(tk); final {
			t.Errorf("a ticket in %q was reported as stopped", status)
		}
	}
}
