package main

import (
	"time"

	"github.com/code-armory-app/blacksmith/internal/agents"
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/roles"
)

// roleToOptions turns a stored role into the agents.Options that builds an agent.
//
// This is the ONE seam between roles-as-data and the agent harness, kept in the
// main package so internal/agents never has to import internal/roles (and the
// gorm dependency it carries). It rebuilds the guard from its serialized form and
// restores the two fields the database holds in a plainer type than Options wants
// — the guard (a func) and the attempt timeout (seconds, not a Duration).
func roleToOptions(r roles.Role) agents.Options {
	class := model.Class(r.Class)
	if class == "" {
		// A role with no class still has to ask the gateway for something; large is
		// what every default role uses and the only class a code stage should run on.
		class = model.ClassLarge
	}
	return agents.Options{
		Name:           r.Name,
		Class:          class,
		Prompt:         r.Prompt,
		Guard:          r.Guard.Build(),
		Tools:          r.Tools,
		TicketKind:     r.TicketKind,
		Check:          r.Check,
		RewriteWhole:   r.RewriteWhole,
		AttemptTimeout: time.Duration(r.AttemptTimeoutSecs) * time.Second,
		Respins:        r.Respins,
		OwnCheck:       r.OwnCheck,
		MaxIterations:  r.MaxIterations,
		Temperature:    r.Temperature,
		MaxTokens:      r.MaxTokens,
		SeedKnown:      r.SeedKnown,
		Thinking:       r.Thinking,
	}
}
