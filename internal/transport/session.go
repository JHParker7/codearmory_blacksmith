package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// Session is a Credential that logs in with an email and password and renews
// when the server says the token has expired.
//
// THIS IS WHAT A PLANE CREDENTIAL IS. The local plane issues short sessions
// rather than long-lived tokens, so a department configured with an email and
// password and no way to renew simply stops working when the first one expires
// — every stage then reports "unauthorized" against a plane that is perfectly
// healthy, which reads as a broken deployment rather than an expired session.
type Session struct {
	loginURL string
	email    string
	password string
	http     *http.Client

	mu    sync.Mutex
	token string

	// inflight is non-nil while a login is running, and is closed when it
	// finishes. See Renew: it is what collapses a herd onto one request.
	inflight chan struct{}
	result   string
	err      error
}

// Login builds a renewing credential.
func Login(loginURL, email, password string, client *http.Client) (*Session, error) {
	if loginURL == "" || email == "" || password == "" {
		return nil, errors.New("session: a login URL, an email and a password are all required")
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &Session{
		loginURL: strings.TrimRight(loginURL, "/"),
		email:    email,
		password: password,
		http:     client,
	}, nil
}

// Token returns the current session, logging in if there is not one yet.
func (s *Session) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	t := s.token
	s.mu.Unlock()
	if t != "" {
		return t, nil
	}
	return s.Renew(ctx)
}

// Renew exchanges the login for a fresh session.
//
// CONCURRENT CALLERS COLLAPSE ONTO ONE LOGIN. Several stages hitting an expired
// token at once should produce ONE request, not one per stage — the gatekeeper
// rate-limits logins, and a thundering herd there locks the whole department out
// of a plane it was already authenticated to.
//
// A CALLER THAT ARRIVES WHILE A LOGIN IS IN FLIGHT WAITS FOR IT and takes its
// result, rather than starting a second. Comparing tokens instead was tried and
// is subtly wrong: a caller that reads the current token AFTER someone else has
// already replaced it sees a value it never used, decides that value is stale,
// and logs in again. What makes two calls one is that they OVERLAP, which is a
// question about timing rather than about which string either of them held.
func (s *Session) Renew(ctx context.Context) (string, error) {
	s.mu.Lock()
	if done := s.inflight; done != nil {
		s.mu.Unlock()

		// THE CALLER'S OWN CONTEXT STILL ENDS THE WAIT. A stage whose ticket was
		// cancelled must not be held here by a login it is no longer interested in.
		select {
		case <-done:
		case <-ctx.Done():
			return "", ctx.Err()
		}

		s.mu.Lock()
		token, err := s.result, s.err
		s.mu.Unlock()
		return token, err
	}

	done := make(chan struct{})
	s.inflight = done
	s.mu.Unlock()

	token, err := s.login(ctx)

	s.mu.Lock()
	s.result, s.err = token, err
	if err == nil {
		s.token = token
	}
	s.inflight = nil
	s.mu.Unlock()
	close(done)

	return token, err
}

// login performs the exchange itself.
func (s *Session) login(ctx context.Context) (string, error) {
	body, err := json.Marshal(map[string]string{"email": s.email, "password": s.password})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.loginURL+"/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("session: login to %s: %w", s.loginURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// THE ADDRESS AND THE BODY ARE BOTH QUOTED, because a login failure is
		// almost always a configuration mistake and the status alone does not say
		// which: a wrong URL answers 404 — which is what pointing this at the forge
		// rather than the gatekeeper gives — and a wrong password answers 401.
		return "", fmt.Errorf("session: login to %s: %s: %s",
			s.loginURL, resp.Status, Snippet(resp.Body))
	}

	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("session: login to %s: decode: %w", s.loginURL, err)
	}
	if out.Token == "" {
		return "", fmt.Errorf("session: login to %s returned no token", s.loginURL)
	}
	return out.Token, nil
}

// Invalidate drops the cached session so the next use logs in again.
func (s *Session) Invalidate() {
	s.mu.Lock()
	s.token = ""
	s.mu.Unlock()
}
