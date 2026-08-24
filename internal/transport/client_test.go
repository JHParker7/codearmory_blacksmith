package transport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// quick builds a client whose backoff does not cost the suite real seconds.
func quick(t *testing.T, h http.Handler, cred Credential) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c := New("test host", srv.URL, cred)
	c.Backoff = time.Millisecond
	return c
}

func TestARequestCarriesTheCredentialAndAcceptsJSON(t *testing.T) {
	var gotAuth, gotAccept, gotType string
	var gotBody string
	c := quick(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotAccept, gotType = r.Header.Get("Authorization"), r.Header.Get("Accept"), r.Header.Get("Content-Type")
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		gotBody = string(b)
		w.Write([]byte(`{"id":"t-1"}`))
	}), Static("tok"))

	var out struct {
		ID string `json:"id"`
	}
	err := c.Do(context.Background(), Request{
		Method: http.MethodPost, Path: "/tickets",
		Body: map[string]string{"title": "x"}, Out: &out,
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotAccept != "application/json" || gotType != "application/json" {
		t.Errorf("Accept = %q, Content-Type = %q", gotAccept, gotType)
	}
	if !strings.Contains(gotBody, `"title":"x"`) {
		t.Errorf("body = %q", gotBody)
	}
	if out.ID != "t-1" {
		t.Errorf("decoded %+v", out)
	}
}

// An empty header value is not sent, so a caller can pass a conditional header
// it may not have a value for without branching around the call.
func TestAnEmptyHeaderIsNotSent(t *testing.T) {
	var sawIfMatch bool
	c := quick(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawIfMatch = r.Header["If-Match"]
	}), Static("tok"))

	err := c.Do(context.Background(), Request{
		Method: http.MethodPut, Path: "/t", Headers: []Header{{"If-Match", ""}},
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if sawIfMatch {
		t.Error("an empty If-Match was sent; the server would compare against nothing")
	}
}

func TestStatusCodesMapToTheClassesCallersSwitchOn(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, ErrDenied},
		{http.StatusForbidden, ErrDenied},
		{http.StatusNotFound, ErrNotFound},
		{http.StatusConflict, ErrConflict},
		{http.StatusPreconditionFailed, ErrConflict},
		{http.StatusInternalServerError, ErrTransient},
		{http.StatusBadGateway, ErrTransient},
	}
	for _, tc := range cases {
		c := quick(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "no", tc.status)
		}), Static("tok"))
		c.MaxRetries = 0

		err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/t"})
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d gave %v, want %v", tc.status, err, tc.want)
		}
	}
}

// A 4xx THAT IS NONE OF THE CLASSES IS NOT TRANSIENT. Retrying a request this
// code got wrong repeats it three more times and reports the last one.
func TestAnUnclassified4xxIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	c := quick(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "the board id is not a uuid", http.StatusBadRequest)
	}), Static("tok"))

	err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/t"})
	if err == nil {
		t.Fatal("a 400 was accepted")
	}
	if Transient(err) {
		t.Error("a 400 was classed as transient")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("the request was issued %d times; a 400 will not become a 200", got)
	}
	// THE SERVER'S OWN WORDS. A message that names the class and not the cause
	// sends a correct reader to the wrong place.
	if !strings.Contains(err.Error(), "the board id is not a uuid") {
		t.Errorf("the response body was discarded: %v", err)
	}
}

func TestATransientFailureIsRetriedAndThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	c := quick(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "later", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{}`))
	}), Static("tok"))

	if err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/t"}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("the request was issued %d times, want 3", got)
	}
}

func TestRetriesAreBoundedAndReportTheLastFailure(t *testing.T) {
	var calls atomic.Int32
	c := quick(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "still down", http.StatusInternalServerError)
	}), Static("tok"))
	c.MaxRetries = 2

	err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/t"})
	if !Transient(err) {
		t.Fatalf("err = %v, want transient", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("the request was issued %d times, want the first plus 2 retries", got)
	}
}

// A cancelled context must stop the retry loop rather than sleeping out the
// remaining backoff — a shutdown that takes the full ladder looks like a hang.
func TestACancelledContextStopsTheRetries(t *testing.T) {
	var calls atomic.Int32
	c := quick(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "down", http.StatusInternalServerError)
	}), Static("tok"))
	c.Backoff = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	err := c.Do(ctx, Request{Method: http.MethodGet, Path: "/t"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the cancellation", err)
	}
	if got := calls.Load(); got > 2 {
		t.Errorf("the request was issued %d times after cancellation", got)
	}
}

// renewing is a Credential whose session expires once.
type renewing struct {
	token    atomic.Value
	renewals atomic.Int32
	fails    bool
}

func newRenewing(first string) *renewing {
	r := &renewing{}
	r.token.Store(first)
	return r
}

func (r *renewing) Token(context.Context) (string, error) { return r.token.Load().(string), nil }

func (r *renewing) Renew(context.Context) (string, error) {
	r.renewals.Add(1)
	if r.fails {
		return "", errors.New("the gatekeeper is unreachable")
	}
	r.token.Store("fresh")
	return "fresh", nil
}

// RENEWAL IS DRIVEN BY THE SERVER'S ANSWER. The host is the authority on whether
// a session is still good, and a timer would need a TTL that is neither
// published nor stable.
func TestADeniedRequestRenewsTheSessionAndRetriesOnce(t *testing.T) {
	var seen []string
	c := quick(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		seen = append(seen, tok)
		if tok != "fresh" {
			http.Error(w, "expired", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{}`))
	}), newRenewing("stale"))

	if err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/t"}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if len(seen) != 2 || seen[0] != "stale" || seen[1] != "fresh" {
		t.Errorf("tokens sent = %q, want the stale one then the renewed one", seen)
	}
}

// EXACTLY ONE RETRY. A genuine permissions failure must surface as a denial
// rather than becoming a login loop against a rate-limited endpoint.
func TestAPermanentDenialIsNotALoginLoop(t *testing.T) {
	var calls atomic.Int32
	cred := newRenewing("stale")
	c := quick(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "no grant", http.StatusForbidden)
	}), cred)

	err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/t"})
	if !Denied(err) {
		t.Fatalf("err = %v, want a denial", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("the request was issued %d times, want the original and one retry", got)
	}
	if got := cred.renewals.Load(); got != 1 {
		t.Errorf("the credential was renewed %d times", got)
	}
}

// A credential that cannot renew — a fixed token from configuration — must
// report the DENIAL, not the renewal failure. The denial is the thing that
// happened; "cannot be renewed" describes the credential, not the problem.
func TestAFixedTokenReportsTheDenialItself(t *testing.T) {
	c := quick(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no grant", http.StatusUnauthorized)
	}), Static("tok"))

	err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/t"})
	if !Denied(err) {
		t.Errorf("err = %v, want a denial", err)
	}
	if strings.Contains(err.Error(), "cannot be renewed") {
		t.Errorf("the error describes the credential rather than the failure: %v", err)
	}
}

func TestARenewalFailureStillReportsTheDenial(t *testing.T) {
	cred := newRenewing("stale")
	cred.fails = true
	c := quick(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "expired", http.StatusUnauthorized)
	}), cred)

	err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/t"})
	if !Denied(err) {
		t.Errorf("err = %v, want a denial", err)
	}
}

// A HOST THAT IS NOT CONFIGURED SAYS SO. Issuing against "" reports a URL parse
// failure, which sends the reader looking for a malformed address rather than a
// missing one.
func TestAnUnconfiguredHostNamesItself(t *testing.T) {
	c := New("forge", "", Static("tok"))
	if c.Configured() {
		t.Error("a client with no base URL reported itself configured")
	}
	err := c.Do(context.Background(), Request{Method: http.MethodPost, Path: "/executions"})
	if err == nil {
		t.Fatal("a request to an unconfigured host succeeded")
	}
	if !strings.Contains(err.Error(), "forge") {
		t.Errorf("the error does not name the host: %v", err)
	}
}

// The response headers are needed BEFORE the body is decoded: the conditional
// write reads the ETag from them, and a claim without it degrades to
// last-write-wins.
func TestOnResponseSeesTheHeaders(t *testing.T) {
	c := quick(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"7"`)
		w.Write([]byte(`{"id":"t-1"}`))
	}), Static("tok"))

	var etag string
	var out struct {
		ID string `json:"id"`
	}
	err := c.Do(context.Background(), Request{
		Method: http.MethodGet, Path: "/t", Out: &out,
		OnResponse: func(h http.Header) { etag = h.Get("ETag") },
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if etag != `"7"` {
		t.Errorf("ETag = %q", etag)
	}
	if out.ID != "t-1" {
		t.Errorf("the body was consumed by the header hook: %+v", out)
	}
}

// A body that is not JSON must fail rather than leaving the caller's struct
// zeroed — an unprefixed path on the platform returns the portal's HTML with a
// 200, and decoding that to an empty list reads as "no work today" rather than
// as a misconfiguration.
func TestAnUndecodableBodyIsAnError(t *testing.T) {
	c := quick(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<!doctype html><html>the portal</html>"))
	}), Static("tok"))

	var out []struct{ ID string }
	err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/tickets", Out: &out})
	if err == nil {
		t.Fatal("HTML decoded as a ticket listing; an empty backlog reads as no work today")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("err = %v, want it to name the decode", err)
	}
}

func TestSnippetBoundsALargeBody(t *testing.T) {
	got := Snippet(strings.NewReader(strings.Repeat("x", 5000)))
	// Tied to the constant rather than to a number typed here: a bound that moves
	// must not need a test edited to agree with it.
	if len(got) > SnippetBytes+len("…") {
		t.Errorf("a 5000-byte body rendered as %d bytes, want at most %d", len(got), SnippetBytes+len("…"))
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("a clipped body does not show it was clipped")
	}
	if got := Snippet(strings.NewReader("  short  ")); got != "short" {
		t.Errorf("Snippet trimmed to %q", got)
	}
}
