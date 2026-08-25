package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// gatekeeper is enough of the plane's login to exercise a session.
type gatekeeper struct {
	mu      sync.Mutex
	logins  int
	issued  []string
	status  int
	body    string
	lastReq map[string]string
}

func (g *gatekeeper) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		g.mu.Lock()
		defer g.mu.Unlock()

		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		g.lastReq = in
		g.logins++

		if g.status != 0 && g.status != http.StatusOK {
			w.WriteHeader(g.status)
			_, _ = w.Write([]byte(g.body))
			return
		}
		token := "session-" + string(rune('a'+g.logins-1))
		g.issued = append(g.issued, token)
		_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (g *gatekeeper) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.logins
}

// A SESSION IS FETCHED ON FIRST USE AND THEN REUSED. Logging in per request
// would rate-limit the department out of a plane it is already authenticated to.
func TestASessionIsFetchedOnceAndReused(t *testing.T) {
	g := &gatekeeper{}
	s, err := Login(g.server(t).URL, "agent@example.test", "hunter2", nil)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	first, err := s.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if first == "" {
		t.Fatal("no token")
	}
	for range 5 {
		again, err := s.Token(context.Background())
		if err != nil || again != first {
			t.Fatalf("Token = %q, %v; want the cached %q", again, err, first)
		}
	}
	if g.count() != 1 {
		t.Errorf("%d logins for six uses", g.count())
	}

	// The credentials reach the gatekeeper in the shape it expects.
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lastReq["email"] != "agent@example.test" || g.lastReq["password"] != "hunter2" {
		t.Errorf("the login sent %v", g.lastReq)
	}
}

// RENEWAL IS DRIVEN BY THE SERVER'S ANSWER, so Renew must actually replace the
// session rather than hand back the one that was just refused.
func TestRenewingReplacesTheSession(t *testing.T) {
	g := &gatekeeper{}
	s, _ := Login(g.server(t).URL, "a@b.test", "pw", nil)

	first, _ := s.Token(context.Background())
	second, err := s.Renew(context.Background())
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if second == first {
		t.Error("Renew handed back the session that had just been refused")
	}
	if got, _ := s.Token(context.Background()); got != second {
		t.Errorf("Token = %q after a renewal to %q", got, second)
	}
}

// CONCURRENT CALLERS COLLAPSE ONTO ONE LOGIN. Several stages hitting an expired
// token at once should produce ONE request: the gatekeeper rate-limits logins,
// and a thundering herd there locks the whole department out.
func TestManyStagesHittingAnExpiredSessionLogInOnce(t *testing.T) {
	g := &gatekeeper{}
	s, _ := Login(g.server(t).URL, "a@b.test", "pw", nil)

	// One session in hand, which every goroutine below then finds stale.
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.count() != 1 {
		t.Fatalf("setup took %d logins", g.count())
	}

	var wg sync.WaitGroup
	got := make([]string, 16)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := s.Renew(context.Background())
			if err != nil {
				t.Errorf("Renew: %v", err)
				return
			}
			got[i] = tok
		}()
	}
	wg.Wait()

	if n := g.count(); n != 2 {
		t.Errorf("%d logins for one expiry seen by 16 stages; want the first plus one renewal", n)
	}
	// And they all end up holding the same session.
	for i, tok := range got {
		if tok != got[0] {
			t.Errorf("stage %d holds %q, stage 0 holds %q", i, tok, got[0])
		}
	}
}

// A LOGIN FAILURE IS ALMOST ALWAYS A CONFIGURATION MISTAKE — a wrong URL answers
// 404 and a wrong password 401 — and the status alone does not say which, so the
// body is quoted.
func TestALoginFailureSaysWhatTheServerSaid(t *testing.T) {
	g := &gatekeeper{status: http.StatusUnauthorized, body: "invalid email or password"}
	s, _ := Login(g.server(t).URL, "a@b.test", "wrong", nil)

	_, err := s.Token(context.Background())
	if err == nil {
		t.Fatal("a rejected login produced a token")
	}
	for _, want := range []string{"401", "invalid email or password"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not carry %q: %v", want, err)
		}
	}
	// AND IT NAMES WHERE IT TRIED, which is the other half of a wrong-URL
	// diagnosis.
	if !strings.Contains(err.Error(), g.server(t).URL[:20]) && !strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("the failure does not say where it logged in: %v", err)
	}
}

// A 404 IS THE WRONG-URL CASE, and it is worth its own test because it is what a
// login pointed at the forge rather than the gatekeeper actually returns.
func TestALoginAtTheWrongAddressSaysSo(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)

	s, _ := Login(srv.URL, "a@b.test", "pw", nil)
	_, err := s.Token(context.Background())
	if err == nil {
		t.Fatal("a 404 produced a token")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("the failure does not name the status: %v", err)
	}
}

// A REPLY WITH NO TOKEN IS A FAILURE, not an empty session that every later
// request is refused for.
func TestALoginThatReturnsNoTokenIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"session":"elsewhere"}`))
	}))
	t.Cleanup(srv.Close)

	s, _ := Login(srv.URL, "a@b.test", "pw", nil)
	if _, err := s.Token(context.Background()); err == nil {
		t.Error("a reply with no token was accepted")
	}
}

// A CREDENTIAL WITH NOTHING TO LOG IN WITH is a configuration error worth
// catching at startup rather than on the first claim.
func TestASessionNeedsAllThreeParts(t *testing.T) {
	for _, c := range []struct{ url, email, pw string }{
		{"", "a@b.test", "pw"},
		{"http://x", "", "pw"},
		{"http://x", "a@b.test", ""},
	} {
		if _, err := Login(c.url, c.email, c.pw, nil); err == nil {
			t.Errorf("Login(%q, %q, %q) was accepted", c.url, c.email, c.pw)
		}
	}
}

// Invalidate drops the session so the next use logs in again — which is how a
// caller that knows a token is dead avoids one guaranteed-refused request.
func TestInvalidateForcesAFreshLogin(t *testing.T) {
	g := &gatekeeper{}
	s, _ := Login(g.server(t).URL, "a@b.test", "pw", nil)

	first, _ := s.Token(context.Background())
	s.Invalidate()
	second, _ := s.Token(context.Background())

	if second == first {
		t.Error("the invalidated session came back")
	}
	if g.count() != 2 {
		t.Errorf("%d logins", g.count())
	}
}

// A TRAILING SLASH ON THE LOGIN URL MUST NOT PRODUCE //login, which some routers
// treat as a different path and answer 404 — the exact failure this whole
// credential exists to avoid being mistaken for a bad password.
func TestATrailingSlashOnTheLoginURLIsHandled(t *testing.T) {
	g := &gatekeeper{}
	s, err := Login(g.server(t).URL+"/", "a@b.test", "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Token(context.Background()); err != nil {
		t.Errorf("a trailing slash broke the login: %v", err)
	}
}

// It satisfies the interface the transport drives renewal through.
func TestASessionIsACredential(t *testing.T) {
	var _ Credential = (*Session)(nil)
}
