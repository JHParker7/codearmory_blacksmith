package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// The CodeArmory client. blacksmith reaches the platform the way a developer's
// tooling does — through conductor, over the public API, with a bearer token —
// so there is no service registration, no shared key and no inbound route here.

// Conductor routes by SERVICE NAME, so every backend path is prefixed with the
// service that owns it: the tickets service's own /tickets collection is reached
// at /tickets/tickets, and its boards at /tickets/boards.
//
// This is worth stating because getting it wrong fails quietly rather than
// loudly: an unprefixed path falls through to the portal, which answers 200 with
// the SPA's HTML. The client then reports a JSON decode error, which reads like
// a malformed API response rather than a wrong URL. Caught by an e2e test — the
// in-process fake had been serving the unprefixed paths back, confirming the
// assumption instead of testing it.
const ticketsService = "/tickets"

// Ticket statuses and priorities, mirroring the tickets service's built-ins.
const (
	StatusOpen       = "open"
	StatusInProgress = "in_progress"
	StatusResolved   = "resolved"
	StatusClosed     = "closed"
)

// Ticket is the subset of the platform's ticket the department reads and writes.
type Ticket struct {
	TicketID    string  `json:"ticket_id"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Status      string  `json:"status"`
	Priority    string  `json:"priority"`
	CreatedBy   string  `json:"created_by"`
	BoardID     *string `json:"board_id,omitempty"`
	AssigneeID  *string `json:"assignee_id,omitempty"`
	ParentID    *string `json:"parent_id,omitempty"`
	// Version is the optimistic-concurrency token. Served as an ETag and sent
	// back as If-Match; see Claim.
	Version  int64     `json:"version"`
	Comments []Comment `json:"comments"`
	// DependsOn is the work this ticket waits for. The tickets service populates
	// it on LISTINGS as well as on reads — unlike comments — which is what lets a
	// stage decide readiness from one poll instead of a fetch per ticket.
	DependsOn []TicketDependency `json:"depends_on"`
	CreatedAt time.Time          `json:"created_at"`
	UpdatedAt time.Time          `json:"updated_at"`
}

// TicketDependency is one prerequisite, carrying enough of the blocker's state
// to judge it without another request. Status is empty when the blocker is not
// visible to this account; see dependenciesMet for why that counts as unmet.
type TicketDependency struct {
	TicketID string `json:"ticket_id"`
	Title    string `json:"title,omitempty"`
	Status   string `json:"status,omitempty"`
}

// Comment is a ticket comment. Comments are append-only with server-assigned
// ids and timestamps, which is what makes them usable as a claim token — see
// Claim.
type Comment struct {
	CommentID string    `json:"comment_id"`
	TicketID  string    `json:"ticket_id"`
	AuthorID  string    `json:"author_id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// TicketUpdate is a partial update. The tickets service treats an omitted field
// as "keep current", so only what is set here changes.
type TicketUpdate struct {
	Title string `json:"title,omitempty"`
	// Description replaces the ticket's body. The product manager uses it to
	// write the specification plan onto the request itself, because an agent is
	// shown a ticket's title, priority and DESCRIPTION and nothing else — a plan
	// left in a comment is a plan no stage can read.
	Description string `json:"description,omitempty"`
	Status      string `json:"status,omitempty"`
	Priority    string `json:"priority,omitempty"`
	// AssigneeID names who holds the ticket. A POINTER so "leave it alone"
	// (nil) is distinguishable from "clear it" (a pointer to ""), which release
	// depends on: a ticket handed back to its queue must not keep the assignee
	// of the agent that gave up on it.
	AssigneeID *string `json:"assignee_id,omitempty"`
}

// ListOpts filters a ticket listing. Empty fields are omitted from the query.
type ListOpts struct {
	BoardID  string
	Status   string
	Priority string
}

// Errors callers must distinguish. The claim path in particular depends on
// telling a lost race from a real failure: treating a lost race as an error
// would make the dispatch loop noisy and eventually stall it.
var (
	// ErrDenied is a 401/403 — the agent account lacks the grant. Not retryable.
	ErrDenied = errors.New("denied")
	// ErrNotFound is a 404. Note the tickets service deliberately returns 404
	// rather than 403 for a ticket the caller cannot see, so existence does not
	// leak; treat "denied" and "gone" as the same outcome for scheduling.
	ErrNotFound = errors.New("not found")
	// ErrConflict is a lost claim race. Expected, not exceptional.
	ErrConflict = errors.New("already claimed")
	// ErrTransient is a 5xx or network failure — retryable.
	ErrTransient = errors.New("transient failure")
)

// isTransient reports a retryable failure. Named rather than inlined because
// three poll loops ask the same question.
func isTransient(err error) bool { return errors.Is(err, ErrTransient) }

// CodeArmory is a client for the platform API.
type CodeArmory struct {
	baseURL string
	// forgeURL, when set, is a forge reached DIRECTLY rather than through
	// conductor — the local forge on the agent host. Empty means sandboxes go to
	// the platform's forge over the normal routed path.
	forgeURL string
	// forgeToken is the credential for the LOCAL sandbox plane, which runs its
	// own gatekeeper and is deliberately not connected to the platform's.
	//
	// Two credentials, not one, and that separation is the point: the agent host
	// holds an ordinary account token for the platform and a local token for its
	// own sandboxes. Compromising the workstation yields the ability to run
	// sandboxes on it — not a platform service key, and not the grants that would
	// come with one. It also means sandboxes keep working when the platform is
	// unreachable, which matters on an intermittent host.
	forgeToken string
	// forgeCred renews the sandbox-plane token when it expires. The plane's
	// gatekeeper issues short-lived sessions, so a token pinned in configuration
	// works until it silently does not.
	forgeCred *Credential
	// ticketsURL, when set, is a ticket store on the SANDBOX PLANE, reached
	// directly. Empty means tickets come from the platform over the routed path.
	// It shares the plane's credential rather than holding one of its own — same
	// gatekeeper, same session. See UseLocalTickets.
	ticketsURL string
	token      string
	http       *http.Client

	// maxRetries bounds retries of transient failures. The control plane is on
	// the always-on side and this host is the intermittent one, but the network
	// between them is not guaranteed either.
	maxRetries int
	backoff    time.Duration
	// pollMin/pollMax bound the sandbox poll loop. Zero means the package
	// defaults; the test suite shrinks them so proving a backoff backs off does
	// not cost real seconds.
	pollMin time.Duration
	pollMax time.Duration

	// leaseSlots caps how many sandboxes this CLIENT holds at once, mirroring the
	// per-user lease quota the forge enforces. It belongs to the client rather
	// than to the process because that is what the quota is scoped to: one client
	// is one user against one forge. Lazily sized — see holdLeaseSlot.
	leaseSlotsOnce sync.Once
	leaseSlots     chan struct{}
}

// NewCodeArmory builds a client. baseURL is the conductor endpoint including
// any path prefix (e.g. https://host/api).
func NewCodeArmory(baseURL, token string) (*CodeArmory, error) {
	if baseURL == "" {
		return nil, errors.New("CODEARMORY_URL is not set")
	}
	if token == "" {
		return nil, errors.New("CODEARMORY_TOKEN is not set: the agent account's credential")
	}
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return nil, fmt.Errorf("CODEARMORY_URL must start with http:// or https:// (got %q)", baseURL)
	}
	return &CodeArmory{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		http:       &http.Client{Timeout: 30 * time.Second},
		maxRetries: 3,
		backoff:    500 * time.Millisecond,
	}, nil
}

// NewStandaloneCodeArmory builds a client with NO platform behind it: tickets and
// sandboxes both come from the plane, and nothing is routed through conductor.
//
// Separate from NewCodeArmory rather than a relaxation of it, because the strict
// check there is worth keeping: a CONNECTED host missing its credential should
// fail at startup with a clear message, not run on and report an empty backlog.
// Standalone is a deliberate mode, so it is chosen deliberately.
func NewStandaloneCodeArmory() *CodeArmory {
	return &CodeArmory{
		http:       &http.Client{Timeout: 30 * time.Second},
		maxRetries: 3,
		backoff:    500 * time.Millisecond,
	}
}

// UseLocalForge points sandbox execution at a forge on this host, bypassing
// conductor. The credential is unchanged: forge verifies the bearer with
// gatekeeper itself, so a local instance authorises exactly as the routed one
// does — being on the same machine grants nothing extra.
func (c *CodeArmory) UseLocalForge(rawURL, token string) error {
	rawURL = strings.TrimRight(strings.TrimSpace(rawURL), "/")
	if rawURL == "" {
		c.forgeURL, c.forgeToken = "", ""
		return nil
	}
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return fmt.Errorf("AGENTS_FORGE_URL must start with http:// or https:// (got %q)", rawURL)
	}
	if token == "" {
		// Falling back to the platform token would be worse than failing: it
		// would send a platform credential to a host that is deliberately not
		// part of the platform's trust domain.
		return errors.New("AGENTS_FORGE_TOKEN is required with AGENTS_FORGE_URL: the local sandbox plane runs its own gatekeeper and does not accept platform tokens")
	}
	c.forgeURL, c.forgeToken = rawURL, token
	return nil
}

// UseRenewingForgeCredential replaces the static sandbox-plane token with one
// that logs in again when it expires.
func (c *CodeArmory) UseRenewingForgeCredential(cred *Credential) { c.forgeCred = cred }

// UseLocalTickets points ticket reads and writes at a store on the SANDBOX PLANE
// rather than at the platform. This is what lets the department run with no
// CodeArmory deployed at all.
//
// It takes no credential of its own, on purpose: the local store authenticates
// against the same gatekeeper as the local forge, so it is the same session.
// Issuing it a second credential would mean two for one trust domain, which can
// drift out of step. That also means the plane must already be configured — a
// local store reached with a PLATFORM token is rejected by a gatekeeper that
// never issued it.
//
// AUTHORITY IS NOT SHARED. With this set the local store IS the authority: there
// is no sync and the platform is not consulted. The alternative — both stores
// writable and reconciled — is master-master on mutable rows, where status and
// priority become last-write-wins and the claim protocol loses the single
// ordering authority it relies on to stop two agents working one ticket.
func (c *CodeArmory) UseLocalTickets(rawURL string) error {
	rawURL = strings.TrimRight(strings.TrimSpace(rawURL), "/")
	if rawURL == "" {
		c.ticketsURL = ""
		return nil
	}
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return fmt.Errorf("AGENTS_TICKETS_URL must start with http:// or https:// (got %q)", rawURL)
	}
	if c.forgeURL == "" {
		return errors.New("AGENTS_TICKETS_URL needs AGENTS_FORGE_URL: the local ticket store shares the sandbox plane's gatekeeper, and without the plane there is no credential it accepts")
	}
	c.ticketsURL = rawURL
	return nil
}

// LocalTickets reports whether tickets come from the sandbox plane.
func (c *CodeArmory) LocalTickets() bool { return c.ticketsURL != "" }

// ticketsPath builds a ticket URL for whichever store this client talks to.
//
// Same trap as forge: through conductor the platform routes by SERVICE NAME, so
// the path carries the /tickets prefix; against a local store nothing routes by
// service name and the prefix is simply wrong. The failure is not symmetrical
// though — an unprefixed path on the PLATFORM does not 404, it falls through to
// the portal and returns the SPA's HTML with a 200, which decodes to an empty
// ticket list and reads as "no work today" rather than as a misconfiguration.
func (c *CodeArmory) ticketsPath(suffix string) string {
	if c.ticketsURL != "" {
		return suffix
	}
	return ticketsService + suffix
}

// planeToken returns the current sandbox-plane session, renewing it when the
// credential is one that can.
func (c *CodeArmory) planeToken(ctx context.Context) (string, error) {
	if c.forgeCred == nil {
		return c.forgeToken, nil
	}
	t, err := c.forgeCred.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("plane credential: %w", err)
	}
	return t, nil
}

// doTickets issues a request against whichever ticket store is configured, with
// the credential that store accepts.
func (c *CodeArmory) doTickets(ctx context.Context, method, path string, body, out any) error {
	return c.doTicketsWithResponse(ctx, method, path, body, out, nil)
}

// doTicketsWithResponse is doTickets with access to the response headers, which
// the claim needs in order to read the ETag.
func (c *CodeArmory) doTicketsWithResponse(ctx context.Context, method, path string, body, out any, onHeader func(http.Header), extra ...header) error {
	if c.ticketsURL == "" {
		return c.doWithResponse(ctx, method, path, body, out, onHeader, extra...)
	}
	token, err := c.planeToken(ctx)
	if err != nil {
		return err
	}
	err = c.doAt(ctx, c.ticketsURL, token, method, path, body, out, onHeader, extra...)
	if err == nil || c.forgeCred == nil || !errors.Is(err, ErrDenied) {
		return err
	}
	// Renew on a 401 and retry ONCE, exactly as the sandbox path does: the server
	// is the authority on whether a session is still valid, and a timer would need
	// a TTL that is neither published nor stable. One retry, so a genuine
	// permissions failure surfaces as a denial instead of a login loop.
	c.forgeCred.Invalidate()
	fresh, rerr := c.forgeCred.Renew(ctx)
	if rerr != nil {
		return fmt.Errorf("plane credential expired and renewal failed: %w (original: %v)", rerr, err)
	}
	return c.doAt(ctx, c.ticketsURL, fresh, method, path, body, out, onHeader, extra...)
}

// doForge issues a request against whichever forge is configured.
//
// The base URL is threaded through rather than swapped on the client. Agents run
// concurrently — the developer agent submits sandboxes while the product manager
// is reading tickets — so temporarily mutating a shared field would send one
// agent's request to the other's host, intermittently and unreproducibly.
func (c *CodeArmory) doForge(ctx context.Context, method, path string, body, out any) error {
	if c.forgeURL == "" {
		return c.do(ctx, method, path, body, out)
	}

	token := c.forgeToken
	if c.forgeCred != nil {
		t, err := c.forgeCred.Token(ctx)
		if err != nil {
			return fmt.Errorf("sandbox credential: %w", err)
		}
		token = t
	}

	err := c.doAt(ctx, c.forgeURL, token, method, path, body, out, nil)
	if err == nil || c.forgeCred == nil || !errors.Is(err, ErrDenied) {
		return err
	}

	// Renew on a 401 and retry ONCE. Driven by the server's answer rather than a
	// clock: the server is the authority on whether a token is still good, and a
	// timer would need a TTL that is not published and can change. Exactly one
	// retry, so a genuine permissions failure surfaces as a denial instead of
	// becoming a login loop against a rate-limited endpoint.
	c.forgeCred.Invalidate()
	fresh, rerr := c.forgeCred.Renew(ctx)
	if rerr != nil {
		return fmt.Errorf("sandbox credential expired and renewal failed: %w (original: %v)", rerr, err)
	}
	return c.doAt(ctx, c.forgeURL, fresh, method, path, body, out, nil)
}

// ListTickets returns tickets matching opts.
func (c *CodeArmory) ListTickets(ctx context.Context, opts ListOpts) ([]Ticket, error) {
	q := url.Values{}
	if opts.BoardID != "" {
		q.Set("board_id", opts.BoardID)
	}
	if opts.Status != "" {
		q.Set("status", opts.Status)
	}
	if opts.Priority != "" {
		q.Set("priority", opts.Priority)
	}
	path := c.ticketsPath("/tickets")
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out []Ticket
	if err := c.doTickets(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetTicket fetches one ticket including its comments.
func (c *CodeArmory) GetTicket(ctx context.Context, id string) (Ticket, error) {
	var out Ticket
	err := c.doTickets(ctx, http.MethodGet, c.ticketsPath("/tickets/"+url.PathEscape(id)), nil, &out)
	return out, err
}

// UpdateTicket applies a partial update.
func (c *CodeArmory) UpdateTicket(ctx context.Context, id string, up TicketUpdate) (Ticket, error) {
	var out Ticket
	err := c.doTickets(ctx, http.MethodPut, c.ticketsPath("/tickets/"+url.PathEscape(id)), up, &out)
	return out, err
}

// CreateTicket opens a ticket. Used by the PM agent's decomposition path later,
// and by the e2e suite to create its own fixtures.
func (c *CodeArmory) CreateTicket(ctx context.Context, t Ticket) (Ticket, error) {
	body := map[string]any{"title": t.Title, "description": t.Description}
	if t.Priority != "" {
		body["priority"] = t.Priority
	}
	// The COLUMN a ticket opens in matters: a child the product manager creates
	// belongs in ready_for_dev, not back in the inbox it was scoped from.
	if t.Status != "" {
		body["status"] = t.Status
	}
	// ParentID keeps a child findable from the request that produced it, which is
	// the whole reason a request survives being broken down.
	if t.ParentID != nil && *t.ParentID != "" {
		body["parent_id"] = *t.ParentID
	}
	if t.BoardID != nil {
		body["board_id"] = *t.BoardID
	}
	var out Ticket
	err := c.doTickets(ctx, http.MethodPost, c.ticketsPath("/tickets"), body, &out)
	return out, err
}

// DeleteTicket soft-deletes a ticket. The e2e suite uses it to clean up after
// itself; nothing in the agent path calls it, and nothing should — an agent that
// can delete tickets can erase the record of what it did.
func (c *CodeArmory) DeleteTicket(ctx context.Context, id string) error {
	return c.doTickets(ctx, http.MethodDelete, c.ticketsPath("/tickets/"+url.PathEscape(id)), nil, nil)
}

// getTicketWithETag reads a ticket and its concurrency token.
//
// An EMPTY etag means the server did not supply one, i.e. it predates optimistic
// concurrency. That is load-bearing: it is how Claim knows to fall back rather
// than issuing a conditional write the server will silently apply anyway.
func (c *CodeArmory) getTicketWithETag(ctx context.Context, id string) (Ticket, string, error) {
	var out Ticket
	var etag string
	err := c.doTicketsWithResponse(ctx, http.MethodGet, c.ticketsPath("/tickets/"+url.PathEscape(id)), nil, &out,
		func(h http.Header) { etag = strings.TrimSpace(h.Get("ETag")) })
	return out, etag, err
}

// updateTicketIfMatch applies a partial update only if the ticket has not
// changed since etag was issued. A lost race surfaces as ErrConflict, mapped
// from the service's 412.
func (c *CodeArmory) updateTicketIfMatch(ctx context.Context, id string, up TicketUpdate, etag string) error {
	return c.doTicketsWithResponse(ctx, http.MethodPut, c.ticketsPath("/tickets/"+url.PathEscape(id)), up, nil, nil,
		header{"If-Match", etag})
}

// AddComment appends a comment.
func (c *CodeArmory) AddComment(ctx context.Context, id, body string) (Comment, error) {
	var out Comment
	err := c.doTickets(ctx, http.MethodPost, c.ticketsPath("/tickets/"+url.PathEscape(id)+"/comments"),
		map[string]string{"body": body}, &out)
	return out, err
}

// claimMarker prefixes the comment that records a claim. It is matched exactly,
// so it must not change without a migration story for in-flight claims.
const claimMarker = "<!-- blacksmith:claim "

// ClaimToken is what a claim comment encodes.
type ClaimToken struct {
	Host  string `json:"host"`
	Role  string `json:"role"`
	RunID string `json:"run_id"`
}

// Claim takes exclusive ownership of a ticket, returning ErrConflict if another
// agent host got there first.
//
// PREFERRED PATH — conditional write. The tickets service serves a version as an
// ETag and accepts If-Match on PUT, so moving the ticket to in_progress is a
// compare-and-set: the version is in the WHERE clause of the UPDATE, so there is
// no window between checking and writing and exactly one racer can win.
//
// FALLBACK — claim by append. Older instances have no If-Match, and there a
// conditional write degrades to last-write-wins, which would let two hosts both
// believe they won. Detected by the absence of an ETag on the read. The fallback
// exploits comments being append-only with server-assigned ordering: every host
// appends a claim, reads back, and yields unless the OLDEST claim is its own.
// Both hosts apply the same deterministic rule to the same server-ordered list,
// so they agree even when one reads before the other has written.
//
// Either way a claim comment is written, because it is also the audit trail and
// the attempt counter: it records which host and role took the work, which the
// agent account alone cannot express when one role runs on several machines.
func (c *CodeArmory) Claim(ctx context.Context, ticketID string, st Stage, tok ClaimToken) error {
	t, etag, err := c.getTicketWithETag(ctx, ticketID)
	if err != nil {
		return fmt.Errorf("claim %s: %w", ticketID, err)
	}
	// THE COLUMN IS THE CLAIM. A ticket in the stage's Ready column is unheld by
	// definition, and taking it means moving it out — so the conditional write
	// below IS the arbitration, not a lock around it. Two hosts that both read
	// the ticket in Ready both try to move it, and the version check means only
	// one succeeds.
	//
	// This replaced a rule that read "claimed" out of the comment history, which
	// had to be status-gated to avoid a ticket looking claimed forever after its
	// first stage. The column says the same thing without the history, and says
	// it to a person looking at the board as well.
	if t.Status != st.Ready {
		return fmt.Errorf("claim %s: %w: status is %s, not %s", ticketID, ErrConflict, t.Status, st.Ready)
	}

	if etag == "" {
		return c.claimByAppend(ctx, ticketID, st, tok)
	}

	holder := tok.Role + "@" + tok.Host
	if err := c.updateTicketIfMatch(ctx, ticketID,
		TicketUpdate{Status: st.Working, AssigneeID: &holder}, etag); err != nil {
		return fmt.Errorf("claim %s: %w", ticketID, err)
	}
	// Won the race. The comment is attribution and the attempt log, not
	// arbitration, so failing to write it does not lose the claim — but it does
	// cost the attempt counter an increment, so it is worth reporting.
	if _, err := c.writeClaimComment(ctx, ticketID, tok); err != nil {
		return fmt.Errorf("claim %s: won but could not record attribution: %w", ticketID, err)
	}
	return nil
}

// claimByAppend is the fallback for instances without If-Match. See Claim.
func (c *CodeArmory) claimByAppend(ctx context.Context, ticketID string, st Stage, tok ClaimToken) error {
	mine, err := c.writeClaimComment(ctx, ticketID, tok)
	if err != nil {
		return fmt.Errorf("claim %s: %w", ticketID, err)
	}

	t, err := c.GetTicket(ctx, ticketID)
	if err != nil {
		// The claim is written but unverifiable. Yielding is the safe direction:
		// a ticket nobody picks up is retried on the next poll, whereas two hosts
		// both proceeding is unrecoverable.
		return fmt.Errorf("claim %s: verify: %w", ticketID, err)
	}
	// Arbitrate against claims from THIS ROLE only.
	//
	// Comparing against every claim on the ticket would mean losing to the stage
	// before: the product manager's claim is older than the developer's and always
	// wins, so the developer never gets a ticket the product manager has scoped.
	// Restricting it to one role is safe because the stages form a pipeline rather
	// than a queue — each takes from a different column, so only one role is ever
	// competing for a given ticket — while still doing the job this protocol
	// exists for: deciding between several HOSTS running that same role.
	winner, ok := oldestClaimForRole(t.Comments, tok.Role)
	if !ok {
		// Our own claim is missing from the read-back — replica lag or a
		// deletion. Same reasoning: yield.
		return fmt.Errorf("claim %s: %w: claim not visible on read-back", ticketID, ErrConflict)
	}
	if winner.CommentID != mine.CommentID {
		return fmt.Errorf("claim %s: %w by %s", ticketID, ErrConflict, winner.AuthorID)
	}
	holder := tok.Role + "@" + tok.Host
	if _, err := c.UpdateTicket(ctx, ticketID, TicketUpdate{Status: st.Working, AssigneeID: &holder}); err != nil {
		return fmt.Errorf("claim %s: move to %s: %w", ticketID, st.Working, err)
	}
	return nil
}

// MoveTo advances a ticket to a column and hands it back, clearing the assignee.
//
// Clearing matters: a ticket that moves on still carrying the last agent's name
// reads, on the board, as though that agent is still working it — and the next
// stage's claim would then be the second thing to write the field rather than
// the first, which is exactly the ambiguity the column is meant to remove.
func (c *CodeArmory) MoveTo(ctx context.Context, ticketID, status string) error {
	unassigned := ""
	if _, err := c.UpdateTicket(ctx, ticketID, TicketUpdate{Status: status, AssigneeID: &unassigned}); err != nil {
		return fmt.Errorf("move %s to %s: %w", ticketID, status, err)
	}
	return nil
}

func (c *CodeArmory) writeClaimComment(ctx context.Context, ticketID string, tok ClaimToken) (Comment, error) {
	payload, err := json.Marshal(tok)
	if err != nil {
		return Comment{}, fmt.Errorf("encode claim: %w", err)
	}
	return c.AddComment(ctx, ticketID, claimMarker+string(payload)+" -->")
}

// ClaimedBy reports the winning claim on a ticket, if any.
func ClaimedBy(t Ticket) (ClaimToken, bool) {
	// THE COLUMN decides whether a ticket is held; the comment only records who
	// holds it. Comments are append-only and nothing deletes them, so a ticket
	// carries every claim ever made against it — including the ones already
	// finished and released. Reading "claimed" off that history means a ticket is
	// claimed forever after its first stage, which locks each stage out of the
	// work the one before it just handed over.
	//
	// A working column is where a stage parks a ticket it is holding, so a ticket
	// in one is claimed and a ticket in any other column is not. This is the same
	// invariant the compare-and-set path states — the comment is attribution, not
	// arbitration — now expressed in the one place a person can also see it.
	if !isWorkingColumn(t.Status) {
		return ClaimToken{}, false
	}
	winner, ok := oldestClaim(t.Comments)
	if !ok {
		return ClaimToken{}, false
	}
	tok, err := parseClaim(winner.Body)
	if err != nil {
		return ClaimToken{}, false
	}
	return tok, true
}

// isWorkingColumn reports whether a column is one a stage parks held work in.
// Derived from the routing table rather than listed separately, so a stage added
// there cannot be forgotten here.
func isWorkingColumn(status string) bool {
	for _, st := range stages {
		if st.Working == status {
			return true
		}
	}
	return false
}

// oldestClaim picks the winning claim comment: earliest created_at, breaking
// ties on comment id so every host reaches the same answer.
func oldestClaim(comments []Comment) (Comment, bool) {
	return oldestClaimForRole(comments, "")
}

// oldestClaimForRole is oldestClaim restricted to one role, which is what the
// append protocol arbitrates between: several hosts running the SAME role. An
// empty role considers every claim. See claimByAppend for why the restriction
// matters — across roles the earlier pipeline stage always wins, permanently.
func oldestClaimForRole(comments []Comment, role string) (Comment, bool) {
	var claims []Comment
	for _, cm := range comments {
		if !strings.HasPrefix(cm.Body, claimMarker) {
			continue
		}
		if role != "" {
			tok, err := parseClaim(cm.Body)
			if err != nil || tok.Role != role {
				continue
			}
		}
		claims = append(claims, cm)
	}
	if len(claims) == 0 {
		return Comment{}, false
	}
	sort.Slice(claims, func(i, j int) bool {
		if !claims[i].CreatedAt.Equal(claims[j].CreatedAt) {
			return claims[i].CreatedAt.Before(claims[j].CreatedAt)
		}
		return claims[i].CommentID < claims[j].CommentID
	})
	return claims[0], true
}

func parseClaim(body string) (ClaimToken, error) {
	rest, ok := strings.CutPrefix(body, claimMarker)
	if !ok {
		return ClaimToken{}, errors.New("not a claim comment")
	}
	rest, ok = strings.CutSuffix(strings.TrimSpace(rest), "-->")
	if !ok {
		return ClaimToken{}, errors.New("malformed claim comment")
	}
	var tok ClaimToken
	if err := json.Unmarshal([]byte(strings.TrimSpace(rest)), &tok); err != nil {
		return ClaimToken{}, fmt.Errorf("malformed claim payload: %w", err)
	}
	return tok, nil
}

// header is one extra request header.
type header struct{ name, value string }

// do performs one API call against the platform, retrying transient failures.
func (c *CodeArmory) do(ctx context.Context, method, path string, body, out any) error {
	return c.doWithResponse(ctx, method, path, body, out, nil)
}

// doAt is do() against an explicit base URL.
func (c *CodeArmory) doAt(ctx context.Context, base, token, method, path string, body, out any, onHeader func(http.Header), extra ...header) error {
	// A standalone host has no platform, so anything still routed there is a
	// feature that has no local equivalent — pipelines, today. Say so, rather than
	// issuing a request against "" and reporting a URL parse failure.
	if base == "" {
		return fmt.Errorf("%s %s: no platform is configured on this host (standalone mode); set CODEARMORY_URL to use it", method, path)
	}
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(c.backoff * time.Duration(1<<(attempt-1))):
			}
		}
		err := c.once(ctx, base, token, method, path, body, out, onHeader, extra)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrTransient) {
			return err
		}
		lastErr = err
	}
	return lastErr
}

// doWithResponse is do() plus access to the response headers and extra request
// headers, which the conditional-write path needs.
func (c *CodeArmory) doWithResponse(ctx context.Context, method, path string, body, out any, onHeader func(http.Header), extra ...header) error {
	return c.doAt(ctx, c.baseURL, c.token, method, path, body, out, onHeader, extra...)
}

func (c *CodeArmory) once(ctx context.Context, base, token, method, path string, body, out any, onHeader func(http.Header), extra []header) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	// CARRY THE TRACE ACROSS THE CALL. Without this every service blacksmith talks
	// to starts its own unrelated root span: forge, tickets and gatekeeper all
	// appear in the collector, and none of them appears CONNECTED to the agent
	// that called them. The architecture view then shows three services and no
	// blacksmith, because a service with no edges is not part of any flow — which
	// is exactly backwards, since the agent is what drives all three.
	//
	// It also makes one ticket's work a single trace: stage → sandbox → forge →
	// gatekeeper, in one timeline, which is the view worth having when a stage is
	// slow and it is not obvious which hop is responsible.
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, h := range extra {
		if h.value != "" {
			req.Header.Set(h.name, h.value)
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w: %v", method, path, ErrTransient, err)
	}
	defer resp.Body.Close()
	if onHeader != nil {
		onHeader(resp.Header)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%s %s: %w: %s", method, path, ErrDenied, snippet(resp.Body))
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%s %s: %w", method, path, ErrNotFound)
	case resp.StatusCode == http.StatusConflict, resp.StatusCode == http.StatusPreconditionFailed:
		// 412 is the tickets service refusing a conditional write because
		// somebody else got there first — a lost race, not a failure.
		//
		// THE SERVER'S OWN WORDS ARE KEPT. ErrConflict reads "already claimed",
		// which is written for the ticket race and is actively misleading anywhere
		// else: forge returns 409 for "lease is expired, not ready", and this
		// rendered it as "POST /executions: already claimed" — a phrase describing a
		// concept forge does not have. It cost a search through the wrong service
		// for a claim mechanism that was never there, while the real message was in
		// the body being discarded here.
		return fmt.Errorf("%s %s: %w: %s", method, path, ErrConflict, snippet(resp.Body))
	case resp.StatusCode >= 500:
		return fmt.Errorf("%s %s: %w: %s: %s", method, path, ErrTransient, resp.Status, snippet(resp.Body))
	case resp.StatusCode >= 400:
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, snippet(resp.Body))
	}

	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}

// snippet bounds an error body: these go into log lines, and a failing service
// can return a very large one.
func snippet(r io.Reader) string {
	const max = 400
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return "<unreadable>"
	}
	s := strings.TrimSpace(string(data))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// ── the workflow board ────────────────────────────────────────────────────────

// FieldDef is a board column as the tickets service models it.
type FieldDef struct {
	FieldDefID string `json:"field_def_id,omitempty"`
	BoardID    string `json:"board_id,omitempty"`
	Kind       string `json:"kind"`
	Value      string `json:"value"`
	Label      string `json:"label"`
	Color      string `json:"color,omitempty"`
	Position   int    `json:"position"`
}

// EnsureColumns makes the board's status columns match this department's
// workflow, creating any that are missing.
//
// Done at STARTUP rather than by hand, because the columns are not decoration:
// they are the routing table, and a stage whose Ready column does not exist
// polls a status the server rejects and quietly never works. A department that
// provisions its own board also stays true to running standalone — bringing up
// the local plane is already `apply the manifests`, and needing someone to
// hand-create twelve columns afterwards would be one more thing to get wrong at
// three in the morning.
//
// ADDITIVE ONLY. Columns that already exist are left exactly as they are, and
// columns this department does not know about are never removed: a person who
// has added their own column to the board means it, and a startup path that
// deleted work-in-progress columns would be a data-loss bug wearing the clothes
// of a reconciler.
func (c *CodeArmory) EnsureColumns(ctx context.Context, boardID string) error {
	if boardID == "" {
		return nil
	}
	var existing []FieldDef
	path := c.ticketsPath("/field-defs") + "?board_id=" + url.QueryEscape(boardID)
	if err := c.doTickets(ctx, http.MethodGet, path, nil, &existing); err != nil {
		return fmt.Errorf("list columns: %w", err)
	}
	have := map[string]bool{}
	for _, f := range existing {
		if f.Kind == fieldKindStatus {
			have[f.Value] = true
		}
	}

	var added int
	for i, col := range workflowColumns {
		if have[col.Value] {
			continue
		}
		body := FieldDef{
			BoardID: boardID, Kind: fieldKindStatus,
			Value: col.Value, Label: col.Label, Color: col.Color, Position: i,
		}
		if err := c.doTickets(ctx, http.MethodPost, c.ticketsPath("/field-defs"), body, nil); err != nil {
			return fmt.Errorf("create column %s: %w", col.Value, err)
		}
		added++
	}
	if added > 0 {
		slog.InfoContext(ctx, "provisioned workflow columns on the board",
			"board_id", boardID, "added", added, "total", len(workflowColumns))
	}
	return nil
}

const fieldKindStatus = "status"

// AddDependency records that one ticket must wait for another.
func (c *CodeArmory) AddDependency(ctx context.Context, ticketID, dependsOn string) error {
	body := map[string]string{"depends_on": dependsOn}
	path := c.ticketsPath("/tickets/" + url.PathEscape(ticketID) + "/dependencies")
	if err := c.doTickets(ctx, http.MethodPost, path, body, nil); err != nil {
		return fmt.Errorf("add dependency %s -> %s: %w", ticketID, dependsOn, err)
	}
	return nil
}

// CreateBoard makes a board and returns its id.
//
// Here so the window can add a project without sending someone to another tool
// for a board id first. That round trip is what makes adding a project feel like
// configuration rather than use, and pasting an id between two places is a step
// with nothing to check it.
func (c *CodeArmory) CreateBoard(ctx context.Context, name string) (string, error) {
	var out struct {
		BoardID string `json:"board_id"`
	}
	if err := c.doTickets(ctx, http.MethodPost, c.ticketsPath("/boards"),
		map[string]string{"name": name}, &out); err != nil {
		return "", fmt.Errorf("create board %q: %w", name, err)
	}
	if out.BoardID == "" {
		return "", fmt.Errorf("create board %q: the store returned no id", name)
	}
	return out.BoardID, nil
}
