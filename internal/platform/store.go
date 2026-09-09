// Package platform is the ticket store: reading the board, writing to it, and
// claiming work off it.
//
// THERE ARE TWO STORES AND THEY ARE NOT INTERCHANGEABLE. One is the platform,
// reached through conductor, which routes by SERVICE NAME — so every path
// carries a /tickets prefix. The other is a store on this host's sandbox plane,
// where nothing routes by service name and the prefix is simply wrong.
//
// The failure is not symmetrical, which is why the prefix is fixed by the
// constructor rather than decided at each call. An unprefixed path against the
// PLATFORM does not 404: it falls through to the portal and returns the web
// application's HTML with a 200, which decodes to an empty ticket list and reads
// as "no work today" rather than as a misconfiguration.
package platform

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/transport"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// servicePrefix is what conductor routes on.
const servicePrefix = "/tickets"

// Store talks to one ticket store.
type Store struct {
	http *transport.Client

	// prefix is set by the constructor and never changes. See the package
	// comment: this is the difference between the two stores, and the one place
	// it can be got wrong is here.
	prefix string

	// now is the clock the claim protocol judges staleness against. Injectable
	// because the behaviour is entirely about where a timestamp falls relative to
	// the present, which is not testable against the real clock.
	now func() time.Time
}

// clock is the store's notion of the present, defaulting to the real one.
func (s *Store) clock() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// Routed builds a store reached through conductor — the platform.
func Routed(baseURL string, cred transport.Credential) (*Store, error) {
	if err := checkURL("CODEARMORY_URL", baseURL); err != nil {
		return nil, err
	}
	return &Store{http: transport.New("platform", baseURL, cred), prefix: servicePrefix}, nil
}

// Local builds a store on this host's sandbox plane, reached directly.
//
// It takes the PLANE's credential, not the platform's. The local store
// authenticates against the same gatekeeper as the local forge, so it is the
// same session; issuing it a second credential would mean two for one trust
// domain, which drift out of step.
//
// AUTHORITY IS NOT SHARED. With this in use the local store IS the authority:
// there is no sync and the platform is not consulted. The alternative — both
// stores writable and reconciled — is master-master on mutable rows, where
// status and priority become last-write-wins and the claim protocol loses the
// single ordering authority it relies on to stop two agents working one ticket.
func Local(baseURL string, cred transport.Credential) (*Store, error) {
	if err := checkURL("AGENTS_TICKETS_URL", baseURL); err != nil {
		return nil, err
	}
	return &Store{http: transport.New("ticket store", baseURL, cred)}, nil
}

func checkURL(name, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("%s is not set", name)
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return fmt.Errorf("%s must start with http:// or https:// (got %q)", name, raw)
	}
	return nil
}

func (s *Store) path(suffix string) string { return s.prefix + suffix }

func (s *Store) ticketPath(id string, suffix string) string {
	return s.path("/tickets/" + url.PathEscape(id) + suffix)
}

// List returns the tickets matching opts.
//
// Filtering is SERVER-SIDE, which is what makes the column-as-stage design
// affordable: a stage polls one status and the store returns only that column,
// rather than every ticket on the board being fetched and discarded.
func (s *Store) List(ctx context.Context, opts ticket.ListOpts) ([]ticket.Ticket, error) {
	q := url.Values{}
	for name, value := range map[string]string{
		"board_id": opts.BoardID,
		"status":   opts.Status,
		"priority": opts.Priority,
		"project":  opts.Project,
	} {
		if value != "" {
			q.Set(name, value)
		}
	}
	path := s.path("/tickets")
	if len(q) > 0 {
		path += "?" + q.Encode()
	}

	var out []ticket.Ticket
	if err := s.http.Do(ctx, transport.Request{Method: http.MethodGet, Path: path, Out: &out}); err != nil {
		return nil, err
	}
	return out, nil
}

// Get fetches one ticket INCLUDING ITS COMMENTS, which a listing does not carry.
func (s *Store) Get(ctx context.Context, id string) (ticket.Ticket, error) {
	var out ticket.Ticket
	err := s.http.Do(ctx, transport.Request{Method: http.MethodGet, Path: s.ticketPath(id, ""), Out: &out})
	return out, err
}

// GetWithVersion reads a ticket and its concurrency token.
//
// AN EMPTY TOKEN MEANS THE STORE DID NOT SUPPLY ONE — an instance predating
// optimistic concurrency. That is load-bearing rather than incidental: it is how
// Claim knows to fall back, instead of issuing a conditional write the store
// will silently apply unconditionally.
func (s *Store) GetWithVersion(ctx context.Context, id string) (ticket.Ticket, string, error) {
	var out ticket.Ticket
	var etag string
	err := s.http.Do(ctx, transport.Request{
		Method: http.MethodGet, Path: s.ticketPath(id, ""), Out: &out,
		OnResponse: func(h http.Header) { etag = strings.TrimSpace(h.Get("ETag")) },
	})
	return out, etag, err
}

// Update applies a partial update. Fields left unset are unchanged.
func (s *Store) Update(ctx context.Context, id string, up ticket.Update) (ticket.Ticket, error) {
	var out ticket.Ticket
	err := s.http.Do(ctx, transport.Request{
		Method: http.MethodPut, Path: s.ticketPath(id, ""), Body: up, Out: &out,
	})
	return out, err
}

// UpdateIfVersion applies an update only if the ticket has not changed since the
// token was issued. A lost race comes back as transport.ErrConflict, from the
// store's 412.
func (s *Store) UpdateIfVersion(ctx context.Context, id string, up ticket.Update, version string) error {
	return s.http.Do(ctx, transport.Request{
		Method: http.MethodPut, Path: s.ticketPath(id, ""), Body: up,
		Headers: []transport.Header{{Name: "If-Match", Value: version}},
	})
}

// createRequest is what opening a ticket sends.
//
// A REQUEST TYPE OF ITS OWN, not the ticket itself, because every field here is
// omitted when empty and the ticket's are not. Sending a ticket would post
// "status": "" on a ticket meant to open wherever the store defaults, and an
// empty status is a column no stage polls.
type createRequest struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	// The COLUMN a ticket opens in matters: a child the product manager creates
	// belongs in its stage's queue, not back in the inbox it was scoped from.
	Status   string `json:"status,omitempty"`
	Priority string `json:"priority,omitempty"`
	// ParentID keeps a child findable from the request that produced it, which is
	// the whole reason a request survives being broken down.
	ParentID string `json:"parent_id,omitempty"`
	BoardID  string `json:"board_id,omitempty"`
	// Project is the free-text project slug the tickets service filters a listing
	// by. Without it a filed ticket lands unfiled and no project-scoped listing
	// (the read_tickets tool) can find it.
	Project string `json:"project,omitempty"`
}

// Create opens a ticket.
func (s *Store) Create(ctx context.Context, t ticket.Ticket) (ticket.Ticket, error) {
	body := createRequest{
		Title: t.Title, Description: t.Description,
		Status: t.Status, Priority: t.Priority,
		ParentID: t.Parent(), BoardID: t.Board(),
		Project: t.Project,
	}
	var out ticket.Ticket
	err := s.http.Do(ctx, transport.Request{
		Method: http.MethodPost, Path: s.path("/tickets"), Body: body, Out: &out,
	})
	return out, err
}

// Delete soft-deletes a ticket.
//
// Here for the end-to-end suite, which cleans up after itself. NOTHING IN THE
// AGENT PATH CALLS IT, and nothing should: an agent that can delete tickets can
// erase the record of what it did.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.http.Do(ctx, transport.Request{Method: http.MethodDelete, Path: s.ticketPath(id, "")})
}

// AddComment appends a comment. Comments are append-only with store-assigned
// ids and ordering, which is what makes them usable as a claim token.
func (s *Store) AddComment(ctx context.Context, id, body string) (ticket.Comment, error) {
	var out ticket.Comment
	err := s.http.Do(ctx, transport.Request{
		Method: http.MethodPost, Path: s.ticketPath(id, "/comments"),
		Body: map[string]string{"body": body}, Out: &out,
	})
	return out, err
}

// AddDependency records that one ticket must wait for another.
func (s *Store) AddDependency(ctx context.Context, id, dependsOn string) error {
	err := s.http.Do(ctx, transport.Request{
		Method: http.MethodPost, Path: s.ticketPath(id, "/dependencies"),
		Body: map[string]string{"depends_on": dependsOn},
	})
	if err != nil {
		return fmt.Errorf("add dependency %s -> %s: %w", id, dependsOn, err)
	}
	return nil
}

// MoveTo advances a ticket to a column and hands it back, CLEARING THE ASSIGNEE.
//
// Clearing matters. A ticket that moves on still carrying the last agent's name
// reads, on the board, as though that agent is still working it — and the next
// stage's claim would then be the second thing to write the field rather than
// the first, which is exactly the ambiguity the column exists to remove.
func (s *Store) MoveTo(ctx context.Context, id, column string) error {
	unassigned := ""
	if _, err := s.Update(ctx, id, ticket.Update{Status: column, AssigneeID: &unassigned}); err != nil {
		return fmt.Errorf("move %s to %s: %w", id, column, err)
	}
	return nil
}

// fieldDef is a board column as the store models it.
type fieldDef struct {
	FieldDefID string `json:"field_def_id,omitempty"`
	BoardID    string `json:"board_id,omitempty"`
	Kind       string `json:"kind"`
	Value      string `json:"value"`
	Label      string `json:"label"`
	Color      string `json:"color,omitempty"`
	Position   int    `json:"position"`
}

const fieldKindStatus = "status"

// EnsureColumns makes a board's status columns match this department's workflow,
// creating any that are missing.
//
// Done at STARTUP rather than by hand, because the columns are not decoration:
// they are the routing table. A stage whose Ready column does not exist polls a
// status the store rejects and quietly never works. A department that provisions
// its own board also stays true to running standalone — bringing up the local
// plane is already "apply the manifests", and needing someone to hand-create
// two dozen columns afterwards would be one more thing to get wrong at three in
// the morning.
//
// ADDITIVE ONLY. Columns that already exist are left exactly as they are, and
// columns this department does not know about are never removed: a person who
// has added their own column to the board means it, and a startup path that
// deleted work-in-progress columns would be a data-loss bug wearing the clothes
// of a reconciler.
func (s *Store) EnsureColumns(ctx context.Context, boardID string) error {
	if boardID == "" {
		return nil
	}

	var existing []fieldDef
	err := s.http.Do(ctx, transport.Request{
		Method: http.MethodGet,
		Path:   s.path("/field-defs") + "?board_id=" + url.QueryEscape(boardID),
		Out:    &existing,
	})
	if err != nil {
		return fmt.Errorf("list columns: %w", err)
	}

	have := map[string]bool{}
	for _, f := range existing {
		if f.Kind == fieldKindStatus {
			have[f.Value] = true
		}
	}

	columns := workflow.Columns()
	var added int
	for i, col := range columns {
		if have[col.Value] {
			continue
		}
		err := s.http.Do(ctx, transport.Request{
			Method: http.MethodPost, Path: s.path("/field-defs"),
			Body: fieldDef{
				BoardID: boardID, Kind: fieldKindStatus,
				Value: col.Value, Label: col.Label, Color: col.Color, Position: i,
			},
		})
		if err != nil {
			return fmt.Errorf("create column %s: %w", col.Value, err)
		}
		added++
	}
	if added > 0 {
		slog.InfoContext(ctx, "provisioned workflow columns on the board",
			"board_id", boardID, "added", added, "total", len(columns))
	}
	return nil
}
