//go:build ticketsmoke

package main

// A LIVE probe of the plane's ticket store, run by hand:
//
//	go test -tags ticketsmoke -run TicketSmoke -v .
//
// It exists because the env file's comment says the HOST-scoped account is
// "list/get/update/comment only", and the security agent is about to depend
// on CREATE. A tag rather than a normal test: it talks to a real deployment.

import (
	"context"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/platform"
	"github.com/code-armory-app/blacksmith/internal/ticket"
)

func TestTicketSmoke(t *testing.T) {
	config.LoadOperatorEnv()
	cfg, err := config.Load()
	if err != nil || cfg.TicketsURL == "" {
		t.Skipf("no ticket store configured: %v", err)
	}
	store, err := platform.Local(cfg.TicketsURL, planeCredential(cfg))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	created, err := store.Create(ctx, ticket.Ticket{
		Title:       "smoke: workshop ticket probe",
		Description: "created by the tickets smoke test; deleted by it too",
		Status:      ticket.StatusOpen,
		Priority:    "low",
	})
	if err != nil {
		t.Fatalf("CREATE refused — the security agent cannot file findings here: %v", err)
	}
	t.Logf("created %s", created.ID)
	if _, err := store.Update(ctx, created.ID, ticket.Update{Status: ticket.StatusClosed}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := store.Delete(ctx, created.ID); err != nil {
		t.Logf("delete refused (fine, closed instead): %v", err)
	}
}
