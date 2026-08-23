package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// A credential that renews itself.
//
// The sandbox plane's gatekeeper issues SHORT-LIVED session tokens — an hour on
// the deployment this was built against — and its long-lived API-token endpoint
// does not exist in every published image. Pinning a token in configuration
// therefore produces a host that works until it quietly does not, hours later,
// with 401s that look like a permissions problem rather than an expiry.
//
// So blacksmith renews instead. It holds the login credential and exchanges it
// for a session on demand, which needs no knowledge of the token's lifetime and
// works against any gatekeeper version. The alternative — a background timer —
// would need the TTL, and would still be wrong the first time someone changed it.
//
// RENEWAL IS DRIVEN BY A 401, not by a clock. The server is the authority on
// whether a token is still good, and asking it is the only check that cannot
// drift.
type Credential struct {
	// Static is a pre-issued token. When set, nothing is renewed — this is the
	// right shape for a long-lived API token where one is available.
	Static string

	loginURL string
	email    string
	password string

	mu    sync.RWMutex
	token string
	http  *http.Client
}

// NewCredential builds a renewing credential. A static token short-circuits
// everything; otherwise loginURL/email/password are exchanged on demand.
func NewCredential(static, loginURL, email, password string, client *http.Client) (*Credential, error) {
	if static != "" {
		return &Credential{Static: static, token: static}, nil
	}
	if loginURL == "" || email == "" || password == "" {
		return nil, errors.New("credential: need either a token or a login URL, email and password")
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &Credential{
		loginURL: strings.TrimRight(loginURL, "/"),
		email:    email, password: password, http: client,
	}, nil
}

// Token returns the current token, logging in if there is not one yet.
func (c *Credential) Token(ctx context.Context) (string, error) {
	c.mu.RLock()
	t := c.token
	c.mu.RUnlock()
	if t != "" {
		return t, nil
	}
	return c.Renew(ctx)
}

// Renew exchanges the login credential for a fresh session.
//
// Concurrent callers collapse onto one login: several agents hitting an expired
// token at once should produce one request, not one per agent — gatekeeper rate
// limits logins, and a thundering herd there locks everyone out.
func (c *Credential) Renew(ctx context.Context) (string, error) {
	if c.Static != "" {
		return c.Static, nil // nothing to renew; the caller's 401 is a real denial
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	body, err := json.Marshal(map[string]string{"email": c.email, "password": c.password})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.loginURL+"/login", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("credential: login: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("credential: login: %s: %s", resp.Status, snippet(resp.Body))
	}

	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("credential: login: decode: %w", err)
	}
	if out.Token == "" {
		return "", errors.New("credential: login returned no token")
	}
	c.token = out.Token
	return out.Token, nil
}

// Invalidate drops the cached token so the next use logs in again.
func (c *Credential) Invalidate() {
	if c.Static != "" {
		return
	}
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}
