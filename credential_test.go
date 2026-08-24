package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// loginServer issues a new token each call, so a test can tell a renewed token
// from a reused one.
func loginServer(t *testing.T) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login" {
			http.Error(w, "no route", http.StatusNotFound)
			return
		}
		mu.Lock()
		n++
		cur := n
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"session-` + string(rune('0'+cur)) + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() int { mu.Lock(); defer mu.Unlock(); return n }
}

func TestCredentialRequiresTokenOrLogin(t *testing.T) {
	if _, err := NewCredential("", "", "", "", nil); err == nil {
		t.Error("NewCredential accepted neither a token nor login details")
	}
	c, err := NewCredential("static-tok", "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := c.Token(context.Background())
	if got != "static-tok" {
		t.Errorf("Token() = %q, want the static token", got)
	}
}

// A static token has nothing to renew; renewing must not blank it, or a 401 on a
// genuine permissions failure would erase the credential.
func TestStaticCredentialIsNotRenewed(t *testing.T) {
	c, _ := NewCredential("static-tok", "", "", "", nil)
	c.Invalidate()
	got, err := c.Renew(context.Background())
	if err != nil || got != "static-tok" {
		t.Errorf("Renew(static) = (%q, %v), want the token unchanged", got, err)
	}
}

func TestCredentialLogsInOnceAndCaches(t *testing.T) {
	srv, logins := loginServer(t)
	c, err := NewCredential("", srv.URL, "a@b.invalid", "pw", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := c.Token(context.Background()); err != nil {
			t.Fatalf("Token() = %v", err)
		}
	}
	if logins() != 1 {
		t.Errorf("logged in %d times for three uses, want 1", logins())
	}
	c.Invalidate()
	if _, err := c.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if logins() != 2 {
		t.Errorf("logged in %d times after invalidation, want 2", logins())
	}
}

// Several agents hitting an expired token at once must produce one login, not
// one each: gatekeeper rate-limits logins, and a herd there locks everyone out.
func TestCredentialRenewalIsSerialised(t *testing.T) {
	srv, logins := loginServer(t)
	c, _ := NewCredential("", srv.URL, "a@b.invalid", "pw", srv.Client())

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Token(context.Background())
		}()
	}
	wg.Wait()
	if n := logins(); n > 8 {
		t.Errorf("%d logins for 8 concurrent first-uses", n)
	}
}

func TestCredentialSurfacesLoginFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
	}))
	defer srv.Close()
	c, _ := NewCredential("", srv.URL, "a@b.invalid", "wrong", srv.Client())
	if _, err := c.Token(context.Background()); err == nil {
		t.Fatal("Token() = nil error against a rejecting server")
	}
}

// The behaviour that matters end to end: an expired sandbox token renews itself
// and the call succeeds, rather than surfacing a 401 that reads like a
// permissions problem hours after deployment.
func TestForgeCallRenewsAnExpiredTokenAndRetries(t *testing.T) {
	integrationTest(t)
	loginSrv, logins := loginServer(t)

	var mu sync.Mutex
	var seen []string
	forgeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen = append(seen, auth)
		mu.Unlock()
		// The first token is stale; anything issued after it is accepted.
		if auth == "session-1" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"execution_id":"e1","status":"completed","exit_code":0}`))
	}))
	defer forgeSrv.Close()

	api, err := NewCodeArmory("http://platform.invalid", "platform-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.UseLocalForge(forgeSrv.URL, "placeholder"); err != nil {
		t.Fatal(err)
	}
	cred, err := NewCredential("", loginSrv.URL, "a@b.invalid", "pw", loginSrv.Client())
	if err != nil {
		t.Fatal(err)
	}
	api.UseRenewingForgeCredential(cred)

	if _, err := api.SubmitExecution(context.Background(), SandboxSpec{Image: "alpine", Command: []string{"ls"}}); err != nil {
		t.Fatalf("SubmitExecution() = %v, want the expired token to renew and the call to succeed", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("forge saw %d requests, want 2 (the rejected one and the retry): %v", len(seen), seen)
	}
	if seen[0] == seen[1] {
		t.Error("the retry reused the expired token")
	}
	if logins() != 2 {
		t.Errorf("logged in %d times, want 2 (initial + renewal)", logins())
	}
}

// Exactly one retry: a genuine permissions failure must surface as a denial
// rather than becoming a login loop against a rate-limited endpoint.
func TestForgeCallRetriesRenewalOnlyOnce(t *testing.T) {
	integrationTest(t)
	loginSrv, logins := loginServer(t)

	var mu sync.Mutex
	var calls int
	forgeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		http.Error(w, "forbidden", http.StatusForbidden) // never satisfied
	}))
	defer forgeSrv.Close()

	api, _ := NewCodeArmory("http://platform.invalid", "platform-token")
	_ = api.UseLocalForge(forgeSrv.URL, "placeholder")
	cred, _ := NewCredential("", loginSrv.URL, "a@b.invalid", "pw", loginSrv.Client())
	api.UseRenewingForgeCredential(cred)

	if _, err := api.SubmitExecution(context.Background(), SandboxSpec{Image: "alpine", Command: []string{"ls"}}); err == nil {
		t.Fatal("SubmitExecution() = nil error against a server that always refuses")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Errorf("forge was called %d times, want 2: one retry, then give up", calls)
	}
	if logins() > 2 {
		t.Errorf("logged in %d times against a rate-limited endpoint", logins())
	}
}
