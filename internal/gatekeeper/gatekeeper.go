// Package gatekeeper is blacksmith's client for the CodeArmory gatekeeper: it
// authorises a caller's bearer, and — because blacksmith is on gatekeeper's
// mint allowlist — provisions each agent run a TEMPORARY IDENTITY scoped to that
// caller. A minted role can never exceed the user's own grants (gatekeeper drops
// any permission the user lacks at mint time), so an agent can never push, file,
// or read anything the user could not.
//
// A lean stdlib client on purpose: it speaks the three endpoints blacksmith
// needs (/check_permissions, /internal/workflow-roles, /internal/run-tokens)
// plus key rotation, and pulls none of the SDK's telemetry dependency tree.
package gatekeeper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Client calls gatekeeper as the named service. key() returns the current
// east-west service key, which rotates — hence a func, not a captured string.
type Client struct {
	URL     string
	Service string
	Key     func() string
	HTTP    *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// Subject is the authenticated caller as gatekeeper resolved them. Username is
// the RBAC namespace resources are keyed by; UserID is the stable owner id.
type Subject struct {
	UserID   string
	Username string
	OrgID    string
}

// Permission is one (service, action, resource) a minted role requests. Only
// those the user already holds survive into the role.
type Permission struct {
	Service  string `json:"service"`
	Action   string `json:"action"`
	Resource string `json:"resource"`
}

// Check validates a caller's bearer for one (action, resource) and returns the
// caller. authorized is false for a denied or unauthenticated caller (err is nil
// then — a denial is an answer, not a failure); err is non-nil only when
// gatekeeper could not be reached.
func (c *Client) Check(ctx context.Context, bearer, action, resource string) (sub Subject, authorized bool, err error) {
	if bearer == "" {
		return Subject{}, false, nil
	}
	body, _ := json.Marshal(map[string]string{"service": c.Service, "resource": resource, "action": action})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/check_permissions", bytes.NewReader(body))
	if err != nil {
		return Subject{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := c.http().Do(req)
	if err != nil {
		return Subject{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return Subject{}, false, nil
	}
	if resp.StatusCode >= 500 {
		io.Copy(io.Discard, resp.Body)
		return Subject{}, false, fmt.Errorf("gatekeeper check: status %d", resp.StatusCode)
	}
	var out struct {
		Authorized bool    `json:"authorized"`
		UserID     string  `json:"user_id"`
		Username   string  `json:"username"`
		OrgID      *string `json:"org_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Subject{}, false, err
	}
	org := ""
	if out.OrgID != nil {
		org = *out.OrgID
	}
	return Subject{UserID: out.UserID, Username: out.Username, OrgID: org}, out.Authorized, nil
}

// MintRole provisions a scoped role for a user, requesting exactly perms;
// gatekeeper keeps only the ones the user already holds. workflowID scopes and
// names the role — pass the run id.
func (c *Client) MintRole(ctx context.Context, workflowID, userID, orgID string, perms []Permission) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"workflow_id": workflowID, "user_id": userID, "org_id": orgID, "permissions": perms,
	})
	var out struct {
		RoleID string `json:"role_id"`
	}
	if err := c.serviceCall(ctx, http.MethodPost, "/internal/workflow-roles", body, &out); err != nil {
		return "", err
	}
	return out.RoleID, nil
}

// MintRunToken mints a short-lived session JWT for the user, bound to roleID —
// the agent's temp identity. Returns the bearer and the session id to revoke.
func (c *Client) MintRunToken(ctx context.Context, userID, roleID string) (token, sessionID string, err error) {
	body, _ := json.Marshal(map[string]string{"user_id": userID, "role_id": roleID})
	var out struct {
		Token     string `json:"token"`
		SessionID string `json:"session_id"`
	}
	if err := c.serviceCall(ctx, http.MethodPost, "/internal/run-tokens", body, &out); err != nil {
		return "", "", err
	}
	return out.Token, out.SessionID, nil
}

// Revoke tears down a run's temp identity — the token session and its role — so
// nothing outlives the action. Best-effort; a leaked short-lived role expires.
func (c *Client) Revoke(ctx context.Context, sessionID, roleID string) {
	if sessionID != "" {
		_ = c.serviceCall(ctx, http.MethodDelete, "/internal/run-tokens/"+sessionID, nil, nil)
	}
	if roleID != "" {
		_ = c.serviceCall(ctx, http.MethodDelete, "/internal/workflow-roles/"+roleID, nil, nil)
	}
}

// serviceCall makes one east-west request authenticated with the rotating
// service key.
func (c *Client) serviceCall(ctx context.Context, method, path string, body []byte, out any) error {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Service-Key", c.Service+":"+c.Key())
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// StartKeyRotation rolls the east-west key on an interval and returns an
// accessor for the current one. gatekeeper issues a new key each rotation; a
// captured copy goes stale, so callers hold this func. On a failed rotation the
// current key is kept and the next tick retries.
func StartKeyRotation(ctx context.Context, url, service, initialKey string, interval time.Duration) func() string {
	var mu sync.RWMutex
	current := initialKey
	get := func() string { mu.RLock(); defer mu.RUnlock(); return current }

	rotate := func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/service-accounts/rotate-key", nil)
		if err != nil {
			return
		}
		req.Header.Set("X-Service-Key", service+":"+get())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return
		}
		var out struct {
			Key string `json:"key"`
		}
		if json.NewDecoder(resp.Body).Decode(&out) == nil && out.Key != "" {
			mu.Lock()
			current = out.Key
			mu.Unlock()
		}
	}

	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				rotate()
			}
		}
	}()
	return get
}
