// Package ticket is the vocabulary every other package speaks.
//
// It holds the shape of a ticket as the department reads and writes it, and
// nothing that talks to a network. Keeping it free of the client is what lets
// the workflow table, the agents and the window all depend on the same types
// without any of them depending on each other.
package ticket

import "time"

// The platform's own lifecycle values, which are NOT the board columns. A
// column says which stage owns the work; a status says whether the ticket is
// open at all. See internal/workflow for the columns.
const (
	StatusOpen       = "open"
	StatusInProgress = "in_progress"
	StatusResolved   = "resolved"
	StatusClosed     = "closed"
)

// Ticket is the subset of the platform's ticket the department reads and writes.
type Ticket struct {
	ID          string  `json:"ticket_id"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Status      string  `json:"status"`
	Priority    string  `json:"priority"`

	// Project is the free-text project slug the platform filters a listing by. It
	// carries NO authorization weight (access is still created_by/org), but it is
	// what makes a ticket appear under a project in the portal and in a
	// project-scoped listing — so the PM tags every task it files with it and a
	// builder lists by it. Empty means "unfiled".
	Project string `json:"project,omitempty"`

	CreatedBy string `json:"created_by"`
	BoardID     *string `json:"board_id,omitempty"`
	AssigneeID  *string `json:"assignee_id,omitempty"`
	ParentID    *string `json:"parent_id,omitempty"`

	// Version is the optimistic-concurrency token, served as an ETag and sent
	// back as If-Match. It is what makes claiming a compare-and-set rather than
	// a check followed by a write — see docs/claiming.md.
	Version int64 `json:"version"`

	// Comments are append-only with server-assigned ids and ordering, which is
	// what makes them usable as a claim token when the store has no If-Match.
	Comments []Comment `json:"comments"`

	// DependsOn is the work this ticket waits for.
	//
	// The store populates it on LISTINGS as well as on reads — unlike comments —
	// which is what lets a stage decide readiness from one poll instead of a
	// fetch per ticket.
	DependsOn []Dependency `json:"depends_on"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Board returns the board this ticket lives on, or "" when it carries none.
func (t Ticket) Board() string {
	if t.BoardID == nil {
		return ""
	}
	return *t.BoardID
}

// Parent returns the ticket this one hangs from, or "" when it is a root.
func (t Ticket) Parent() string {
	if t.ParentID == nil {
		return ""
	}
	return *t.ParentID
}

// Dependency is one prerequisite, carrying enough of the blocker's state to
// judge it without another request.
//
// Status is EMPTY when the blocker is not visible to this account, and that
// counts as unmet: a stage cannot prove a prerequisite is finished, so it must
// not assume it is.
type Dependency struct {
	ID     string `json:"ticket_id"`
	Title  string `json:"title,omitempty"`
	Status string `json:"status,omitempty"`
}

// Comment is a ticket comment.
type Comment struct {
	ID        string    `json:"comment_id"`
	TicketID  string    `json:"ticket_id"`
	AuthorID  string    `json:"author_id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// Update is a partial update. The store treats an omitted field as "keep
// current", so only what is set here changes.
type Update struct {
	Title string `json:"title,omitempty"`

	// Description REPLACES the ticket's body. The product manager writes its
	// plan here rather than in a comment, because an agent is shown a ticket's
	// title, priority and description and nothing else — a plan left in a
	// comment is a plan no stage can read.
	Description string `json:"description,omitempty"`

	Status   string `json:"status,omitempty"`
	Priority string `json:"priority,omitempty"`

	// AssigneeID names who holds the ticket.
	//
	// A POINTER, so "leave it alone" (nil) is distinguishable from "clear it" (a
	// pointer to ""). Releasing depends on that: a ticket handed back to its
	// queue must not keep the assignee of the agent that gave up on it.
	AssigneeID *string `json:"assignee_id,omitempty"`
}

// ListOpts filters a listing. Empty fields are omitted from the query.
type ListOpts struct {
	BoardID  string
	Status   string
	Priority string
	Project  string
}

// Ptr is the idiom for the optional string fields above, so callers are not
// each inventing a local helper to take the address of a literal.
//
// An EMPTY string returns nil rather than a pointer to "", because every
// optional field here reads nil as "not set" and a pointer to empty as an
// explicit clear. Handing the store a deliberate clear when the caller meant
// "no value" is a different request.
func Ptr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
