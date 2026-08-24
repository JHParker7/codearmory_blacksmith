package transport

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

// A CLIP MUST CUT ON A RUNE BOUNDARY.
//
// This is the shape of the mistake that bricked a run: a hook counted a subject
// in bytes while the text was clipped by runes, and five specification sections
// blocked with a message pointing squarely at the network. This department's
// prose is full of em dashes — three bytes each — so a byte cut lands
// mid-character most of the time, and a log line that is not valid text is one a
// reader distrusts entirely.
func TestSnippetCutsOnARuneBoundary(t *testing.T) {
	for _, r := range []string{"é", "—", "🔥", "日"} {
		got := Snippet(strings.NewReader(strings.Repeat(r, 500)))
		if !utf8.ValidString(got) {
			t.Errorf("Snippet of repeated %q produced invalid UTF-8", r)
		}
		if !strings.HasSuffix(got, "…") {
			t.Errorf("Snippet of repeated %q was not marked as clipped: %q", r, got)
		}
	}
}

// THE EMPTY AND UNREADABLE CASES ARE NAMED. A message ending in a bare colon
// reads as though it were itself truncated, and "the server said nothing" is
// what stops a reader hunting for a reason in a body that has none.
func TestSnippetNamesABodyThatSaysNothing(t *testing.T) {
	if got := Snippet(strings.NewReader("")); got != "<empty body>" {
		t.Errorf("Snippet(empty) = %q", got)
	}
	if got := Snippet(strings.NewReader("   \n  ")); got != "<empty body>" {
		t.Errorf("Snippet(whitespace) = %q", got)
	}
	if got := Snippet(failingReader{}); got != "<unreadable body>" {
		t.Errorf("Snippet(failing reader) = %q", got)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("the connection dropped") }

// A CLIENT WITH NO CREDENTIAL IS A WIRING MISTAKE and must say so. A nil
// dereference here surfaces as a stack trace from whichever agent happened to
// call first, which names the caller rather than the thing never configured.
func TestAMissingCredentialIsReportedNotPanicked(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Do panicked with no credential: %v", r)
		}
	}()

	c := New("forge", "http://127.0.0.1:1", nil)
	err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x"})
	if err == nil {
		t.Fatal("a request with no credential was attempted")
	}
	if !strings.Contains(err.Error(), "forge") || !strings.Contains(err.Error(), "credential") {
		t.Errorf("err = %v, want it to name the host and what is missing", err)
	}
}

// 204 MEANS THERE IS NOTHING TO DECODE, and that is a success: a store that
// answers a write with no content has done the write. Failing here would turn a
// completed move into an error, and the caller would retry a write that landed.
func TestANoContentAnswerIsASuccess(t *testing.T) {
	c := quick(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), Static("tok"))

	var out struct {
		ID string `json:"ticket_id"`
	}
	if err := c.Do(context.Background(), Request{Method: http.MethodPut, Path: "/t", Out: &out}); err != nil {
		t.Errorf("a 204 with an Out parameter failed: %v", err)
	}
	if out.ID != "" {
		t.Errorf("a 204 decoded something: %+v", out)
	}
}

// ...but an EMPTY 200 still fails, deliberately. That is the shape a misrouted
// request takes, and handing back a zero-valued struct is how "no work today"
// gets reported instead of a misconfiguration.
func TestAnEmptyTwoHundredStillFails(t *testing.T) {
	c := quick(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), Static("tok"))

	var out []struct{ ID string }
	if err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/tickets", Out: &out}); err == nil {
		t.Error("an empty 200 decoded as an empty listing")
	}
}

// An unnamed client still produces a readable message rather than a sentence
// with a hole in it.
func TestAnUnnamedClientStillNamesSomething(t *testing.T) {
	c := &Client{}
	err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x"})
	if err == nil {
		t.Fatal("an unconfigured client issued a request")
	}
	if strings.Contains(err.Error(), "no  is") {
		t.Errorf("err = %v, want a noun where the host name would go", err)
	}
}
