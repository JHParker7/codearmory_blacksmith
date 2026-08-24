package platform

import (
	"context"
	"fmt"

	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/transport"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// Claim takes exclusive ownership of a ticket, returning transport.ErrConflict
// if another host got there first.
//
// PREFERRED PATH — A CONDITIONAL WRITE. The store serves a version as an ETag
// and accepts If-Match on a write, so moving the ticket to the working column is
// a compare-and-set: the version is in the WHERE clause of the update, there is
// no window between checking and writing, and exactly one racer can win.
//
// FALLBACK — CLAIM BY APPEND. Older instances have no If-Match, and there a
// conditional write degrades to last-write-wins, which would let two hosts both
// believe they won. It is detected by the ABSENCE of an ETag on the read. The
// fallback exploits comments being append-only with store-assigned ordering:
// every host appends a claim, reads back, and yields unless the OLDEST claim for
// its role is its own. Both hosts apply the same deterministic rule to the same
// ordered list, so they agree even when one reads before the other has written.
//
// Either way A CLAIM COMMENT IS WRITTEN, because it is also the audit trail and
// the attempt counter: it records which host and role took the work, which the
// agent account alone cannot express when one role runs on several machines.
func (s *Store) Claim(ctx context.Context, id string, st workflow.Stage, c record.Claim) error {
	t, version, err := s.GetWithVersion(ctx, id)
	if err != nil {
		return fmt.Errorf("claim %s: %w", id, err)
	}

	// THE COLUMN IS THE CLAIM. A ticket in the stage's Ready column is unheld by
	// definition, and taking it means moving it OUT — so the conditional write
	// below is the arbitration itself, not a lock around it. Two hosts that both
	// read the ticket in Ready both try to move it, and the version check means
	// only one succeeds.
	//
	// This replaced a rule that read "claimed" out of the comment history, which
	// had to be status-gated to stop a ticket looking claimed forever after its
	// first stage. The column says the same thing without the history, and says it
	// to a person looking at the board as well.
	if t.Status != st.Ready {
		return fmt.Errorf("claim %s: %w: the ticket is in %s, not %s", id, transport.ErrConflict, t.Status, st.Ready)
	}

	if version == "" {
		return s.claimByAppend(ctx, id, st, c)
	}

	holder := c.Role + "@" + c.Host
	if err := s.UpdateIfVersion(ctx, id, updateHeldBy(st, holder), version); err != nil {
		return fmt.Errorf("claim %s: %w", id, err)
	}

	// Won the race. The comment is attribution and the attempt log, NOT
	// arbitration, so failing to write it does not lose the claim — but it does
	// cost the attempt counter an increment, so it is worth reporting.
	if _, err := s.writeClaim(ctx, id, c); err != nil {
		return fmt.Errorf("claim %s: won but could not record attribution: %w", id, err)
	}
	return nil
}

// claimByAppend is the fallback for stores without If-Match. See Claim.
func (s *Store) claimByAppend(ctx context.Context, id string, st workflow.Stage, c record.Claim) error {
	mine, err := s.writeClaim(ctx, id, c)
	if err != nil {
		return fmt.Errorf("claim %s: %w", id, err)
	}

	t, err := s.Get(ctx, id)
	if err != nil {
		// The claim is written but unverifiable. YIELDING IS THE SAFE DIRECTION: a
		// ticket nobody picks up is retried on the next poll, whereas two hosts both
		// proceeding is unrecoverable.
		return fmt.Errorf("claim %s: verify: %w", id, err)
	}

	// Arbitrate against claims from THIS ROLE only — see record.OldestForRole.
	winner, ok := record.OldestForRole(t.Comments, c.Role)
	if !ok {
		// Our own claim is missing from the read-back: replica lag, or a deletion.
		// Same reasoning as above — yield.
		return fmt.Errorf("claim %s: %w: the claim is not visible on read-back", id, transport.ErrConflict)
	}
	if winner.ID != mine.ID {
		return fmt.Errorf("claim %s: %w: %s got there first", id, transport.ErrConflict, winner.AuthorID)
	}

	holder := c.Role + "@" + c.Host
	if _, err := s.Update(ctx, id, updateHeldBy(st, holder)); err != nil {
		return fmt.Errorf("claim %s: move to %s: %w", id, st.Working, err)
	}
	return nil
}

func (s *Store) writeClaim(ctx context.Context, id string, c record.Claim) (ticket.Comment, error) {
	body, err := record.Render(c)
	if err != nil {
		return ticket.Comment{}, err
	}
	return s.AddComment(ctx, id, body)
}

// updateHeldBy is the write that TAKES a ticket: into the stage's working
// column, assigned to the host holding it.
//
// ONE FUNCTION FOR BOTH PATHS, because the two must write exactly the same
// thing. They differ only in whether the write is conditional — a difference in
// arbitration, not in what a held ticket looks like — and letting them build the
// update separately is how a ticket ends up claimed on one path and merely moved
// on the other.
func updateHeldBy(st workflow.Stage, holder string) ticket.Update {
	return ticket.Update{Status: st.Working, AssigneeID: &holder}
}
