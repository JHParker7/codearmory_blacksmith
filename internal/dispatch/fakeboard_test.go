package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/transport"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// board is an in-memory ticket store with the claim protocol's ordering
// properties: comments are append-only with assigned ids and timestamps, and a
// claim moves the ticket only if it is still in the queue.
type board struct {
	mu sync.Mutex

	tickets map[string]*ticket.Ticket
	seq     int
	now     time.Time

	// failGet, failMove and failClaim make the corresponding call fail, so the
	// paths that must survive a partial failure are exercised rather than assumed.
	failGet   map[string]bool
	failMove  bool
	failClaim error

	// beforeClaim runs before a claim is decided, which is how a test puts a
	// competitor's win at exactly the wrong moment.
	beforeClaim func(id string)

	moves []string

	// closedAtMove records, for each move, whether the ticket already carried a
	// claim close as it left. The ORDER is the invariant, not just the presence.
	closedAtMove []bool
}

func newBoard() *board {
	return &board{
		tickets: map[string]*ticket.Ticket{},
		now:     time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		failGet: map[string]bool{},
	}
}

func (b *board) add(t ticket.Ticket) *ticket.Ticket {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t.ID == "" {
		b.seq++
		t.ID = fmt.Sprintf("t-%d", b.seq)
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = b.now
	}
	b.tickets[t.ID] = &t
	return &t
}

func (b *board) get(id string) ticket.Ticket {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.tickets[id]
	if !ok {
		return ticket.Ticket{}
	}
	return *t
}

func (b *board) List(_ context.Context, opts ticket.ListOpts) ([]ticket.Ticket, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []ticket.Ticket
	for _, t := range b.tickets {
		if opts.Status != "" && t.Status != opts.Status {
			continue
		}
		if opts.BoardID != "" && t.Board() != opts.BoardID {
			continue
		}
		// A LISTING CARRIES NO COMMENTS. Modelled, because selection built on a
		// listing silently evaluates against nothing.
		shallow := *t
		shallow.Comments = nil
		out = append(out, shallow)
	}
	return out, nil
}

func (b *board) Get(_ context.Context, id string) (ticket.Ticket, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failGet[id] {
		return ticket.Ticket{}, errors.New("the store could not be reached")
	}
	t, ok := b.tickets[id]
	if !ok {
		return ticket.Ticket{}, fmt.Errorf("%w: %s", transport.ErrNotFound, id)
	}
	return *t, nil
}

// clock reads the board's time UNDER THE LOCK. AddComment advances it, and
// several stages comment concurrently — reading the field directly is a race
// the detector only reaches once two goroutines really do comment at once.
func (b *board) clock() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.now
}

func (b *board) AddComment(_ context.Context, id, body string) (ticket.Comment, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.tickets[id]
	if !ok {
		return ticket.Comment{}, fmt.Errorf("%w: %s", transport.ErrNotFound, id)
	}
	b.seq++
	b.now = b.now.Add(time.Second)
	c := ticket.Comment{
		ID: fmt.Sprintf("c-%d", b.seq), TicketID: id, Body: body, CreatedAt: b.now,
	}
	t.Comments = append(t.Comments, c)
	return c, nil
}

func (b *board) MoveTo(_ context.Context, id, column string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failMove {
		return errors.New("the store could not be reached")
	}
	t, ok := b.tickets[id]
	if !ok {
		return fmt.Errorf("%w: %s", transport.ErrNotFound, id)
	}
	t.Status = column
	t.AssigneeID = nil

	// SAMPLED AS THE TICKET LEAVES, because the ORDER is the invariant. A claim
	// closed after the move would retroactively release a claim another host
	// wrote in between, letting a third host win over one already working.
	closed := false
	for _, cm := range t.Comments {
		if strings.HasPrefix(cm.Body, record.ClaimClosedMarker) {
			closed = true
			break
		}
	}
	b.closedAtMove = append(b.closedAtMove, closed)

	b.moves = append(b.moves, id+"→"+column)
	return nil
}

func (b *board) Claim(_ context.Context, id string, st workflow.Stage, c record.Claim) error {
	if b.beforeClaim != nil {
		b.beforeClaim(id)
	}
	if b.failClaim != nil {
		return b.failClaim
	}

	b.mu.Lock()
	t, ok := b.tickets[id]
	if !ok {
		b.mu.Unlock()
		return fmt.Errorf("%w: %s", transport.ErrNotFound, id)
	}
	// THE COLUMN IS THE CLAIM: a ticket that has moved on is already someone's.
	if t.Status != st.Ready {
		b.mu.Unlock()
		return fmt.Errorf("claim %s: %w: the ticket is in %s", id, transport.ErrConflict, t.Status)
	}
	holder := c.Role + "@" + c.Host
	t.Status, t.AssigneeID = st.Working, &holder
	b.mu.Unlock()

	body, err := record.Render(c)
	if err != nil {
		return err
	}
	_, err = b.AddComment(context.Background(), id, body)
	return err
}

func (b *board) seenMoves() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.moves))
	copy(out, b.moves)
	return out
}

// stubHandler is an agent that reports whatever a test tells it to.
type stubHandler struct {
	role   string
	status workflow.Outcome
	detail string
	err    error

	wants func(ticket.Ticket) bool

	mu      sync.Mutex
	handled []string
	block   chan struct{}
}

func (h *stubHandler) Role() string       { return h.role }
func (h *stubHandler) Class() model.Class { return model.ClassLarge }

func (h *stubHandler) Wants(t ticket.Ticket) bool {
	if h.wants == nil {
		return true
	}
	return h.wants(t)
}

func (h *stubHandler) Handle(_ context.Context, t ticket.Ticket) (workflow.Outcome, string, error) {
	h.mu.Lock()
	h.handled = append(h.handled, t.ID)
	block := h.block
	h.mu.Unlock()

	if block != nil {
		<-block
	}
	if h.status == "" {
		return workflow.OutcomeSuccess, h.detail, h.err
	}
	return h.status, h.detail, h.err
}

func (h *stubHandler) worked() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.handled))
	copy(out, h.handled)
	return out
}

// newDispatcher wires a dispatcher for the developer stage against a board.
func newDispatcher(t *testing.T, b *board, h Handler, opts Options) *Dispatcher {
	t.Helper()
	if opts.Host == "" {
		opts.Host = "gpu-1"
	}
	d, err := New(b, h, workflow.New(workflow.Options{}), opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.now = b.clock
	return d
}

func devStage(t *testing.T) workflow.Stage {
	t.Helper()
	st, ok := workflow.New(workflow.Options{}).For(workflow.RoleDev)
	if !ok {
		t.Fatal("no developer stage")
	}
	return st
}
