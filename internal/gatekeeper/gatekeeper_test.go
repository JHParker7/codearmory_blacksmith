package gatekeeper

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckReturnsTheCaller(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/check_permissions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer usertoken" {
			t.Errorf("forwarded auth = %q, want the caller's bearer", got)
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["action"] != "runAction" || body["resource"] != "blacksmith/actions" {
			t.Errorf("check body = %v", body)
		}
		json.NewEncoder(w).Encode(map[string]any{"authorized": true, "user_id": "u1", "username": "alice"})
	}))
	defer srv.Close()

	c := &Client{URL: srv.URL, Service: "blacksmith", Key: func() string { return "k" }}
	sub, ok, err := c.Check(context.Background(), "usertoken", "runAction", "blacksmith/actions")
	if err != nil || !ok {
		t.Fatalf("Check ok=%v err=%v", ok, err)
	}
	if sub.UserID != "u1" || sub.Username != "alice" {
		t.Fatalf("subject = %+v", sub)
	}

	// An empty bearer is unauthorized without a round-trip.
	if _, ok, _ := c.Check(context.Background(), "", "runAction", "blacksmith/actions"); ok {
		t.Error("empty bearer should not authorize")
	}
}

func TestMintRoleAndRunTokenUseTheServiceKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Service-Key"); got != "blacksmith:secret" {
			t.Errorf("service key header = %q", got)
		}
		switch r.URL.Path {
		case "/internal/workflow-roles":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["user_id"] != "u1" {
				t.Errorf("mint role user_id = %v", body["user_id"])
			}
			json.NewEncoder(w).Encode(map[string]string{"role_id": "role-9"})
		case "/internal/run-tokens":
			json.NewEncoder(w).Encode(map[string]string{"token": "scoped-jwt", "session_id": "sess-9"})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := &Client{URL: srv.URL, Service: "blacksmith", Key: func() string { return "secret" }}
	roleID, err := c.MintRole(context.Background(), "job-1", "u1", "", []Permission{{Service: "tickets", Action: "createTicket", Resource: "tickets/tickets"}})
	if err != nil || roleID != "role-9" {
		t.Fatalf("MintRole = %q, %v", roleID, err)
	}
	tok, sess, err := c.MintRunToken(context.Background(), "u1", roleID)
	if err != nil || tok != "scoped-jwt" || sess != "sess-9" {
		t.Fatalf("MintRunToken = %q, %q, %v", tok, sess, err)
	}
}
