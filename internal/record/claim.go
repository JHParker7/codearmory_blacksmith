package record

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/code-armory-app/blacksmith/internal/ticket"
)

// Claim is what a claim comment encodes: the host and role that took the work,
// and the run it belongs to.
//
// The AGENT ACCOUNT cannot express this. One role runs on several machines under
// the same credentials, so "assigned to the developer" does not say which
// developer, and the append-protocol below has to arbitrate between exactly
// those.
type Claim struct {
	Host  string `json:"host"`
	Role  string `json:"role"`
	RunID string `json:"run_id"`
}

// Render is the comment body for a claim.
func Render(c Claim) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode claim: %w", err)
	}
	return ClaimMarker + string(payload) + " -->", nil
}

// IsClaim reports whether a comment body is a claim record.
func IsClaim(body string) bool {
	return strings.HasPrefix(body, ClaimMarker)
}

// Parse reads a claim back out of a comment body.
func Parse(body string) (Claim, error) {
	rest, ok := strings.CutPrefix(body, ClaimMarker)
	if !ok {
		return Claim{}, errors.New("not a claim comment")
	}
	rest, ok = strings.CutSuffix(strings.TrimSpace(rest), "-->")
	if !ok {
		return Claim{}, errors.New("malformed claim comment")
	}
	var c Claim
	if err := json.Unmarshal([]byte(strings.TrimSpace(rest)), &c); err != nil {
		return Claim{}, fmt.Errorf("malformed claim payload: %w", err)
	}
	return c, nil
}

// CloseClaim is the comment body that ends a role's current claim round. See
// ClaimClosedMarker for why a round needs an end at all.
func CloseClaim(role string) (string, error) {
	payload, err := json.Marshal(Claim{Role: role})
	if err != nil {
		return "", fmt.Errorf("encode claim close: %w", err)
	}
	return ClaimClosedMarker + string(payload) + " -->", nil
}

// closesClaim reports whether a comment ends the claim round for a role. An
// empty role matches any close, because "who holds this ticket" is a question
// about the ticket rather than about one stage.
func closesClaim(body, role string) bool {
	rest, ok := strings.CutPrefix(body, ClaimClosedMarker)
	if !ok {
		return false
	}
	if role == "" {
		return true
	}
	rest, ok = strings.CutSuffix(strings.TrimSpace(rest), "-->")
	if !ok {
		// AN UNREADABLE CLOSE DOES NOT CLOSE. Treating one as a close for every role
		// would silently discard live claims and hand a ticket to a second host.
		return false
	}
	var c Claim
	if err := json.Unmarshal([]byte(strings.TrimSpace(rest)), &c); err != nil {
		return false
	}
	return c.Role == role
}

// Oldest picks the winning claim among a ticket's comments: earliest created_at,
// breaking ties on comment id.
//
// THE TIE-BREAK IS THE WHOLE POINT. This is the arbitration rule of the append
// protocol — every host appends a claim, reads back, and yields unless the
// oldest is its own — and it only works because both hosts apply the SAME
// deterministic rule to the same server-ordered list. Two claims written in the
// same instant with no tie-break would let each host pick itself.
//
// An empty role considers every claim; naming one restricts arbitration to the
// hosts running THAT role. See OldestForRole.
//
// EITHER WAY THE CLAIM MUST BE READABLE. A body that carries the marker but does
// not decode names nobody, and letting one win means a ticket whose column says
// it is held reports no holder at all -- the window then shows it as free while
// an agent is working it. The oldest claim anyone can actually read is the best
// available answer, and it is the only one worth giving.
func Oldest(comments []ticket.Comment) (ticket.Comment, bool) {
	return OldestForRole(comments, "")
}

// OldestForRole is Oldest restricted to one role, which is what the append
// protocol actually arbitrates between: several hosts running the SAME role.
//
// Comparing against every claim on the ticket instead would mean losing to the
// stage BEFORE: the product manager's claim is older than the developer's and
// would always win, so the developer never gets a ticket the product manager has
// scoped. Restricting to one role is safe because the stages form a pipeline
// rather than a queue — each takes from a different column, so only one role is
// ever competing for a given ticket.
func OldestForRole(comments []ticket.Comment, role string) (ticket.Comment, bool) {
	// SORTED FIRST, because the answer now depends on which side of a close each
	// claim falls on, and file order is not a documented guarantee. Attempts sorts
	// for the same reason.
	ordered := make([]ticket.Comment, len(comments))
	copy(ordered, comments)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].CreatedAt.Equal(ordered[j].CreatedAt) {
			return ordered[i].CreatedAt.Before(ordered[j].CreatedAt)
		}
		return ordered[i].ID < ordered[j].ID
	})

	var claims []ticket.Comment
	for _, cm := range ordered {
		// A CLOSED ROUND IS NOT ARBITRATED. Claims before this point belong to
		// attempts that have already ended; leaving them in means every retry loses
		// to a claim its own previous attempt wrote. See ClaimClosedMarker.
		if closesClaim(cm.Body, role) {
			claims = nil
			continue
		}
		if !IsClaim(cm.Body) {
			continue
		}
		c, err := Parse(cm.Body)
		if err != nil {
			// Unreadable. It cannot be shown to belong to this role, and it cannot
			// name a holder, so it takes part in neither question.
			continue
		}
		if role != "" && c.Role != role {
			continue
		}
		claims = append(claims, cm)
	}
	if len(claims) == 0 {
		return ticket.Comment{}, false
	}
	sort.Slice(claims, func(i, j int) bool {
		if !claims[i].CreatedAt.Equal(claims[j].CreatedAt) {
			return claims[i].CreatedAt.Before(claims[j].CreatedAt)
		}
		return claims[i].ID < claims[j].ID
	})
	return claims[0], true
}

// HeldBy reports the claim standing on a ticket, if any.
//
// held answers whether the ticket's column is one a stage parks HELD work in —
// see workflow.Table.IsWorking. The column decides whether a ticket is claimed;
// the comment only records who claimed it. Comments are append-only and nothing
// deletes them, so reading "claimed" off the history alone would mean a ticket
// is claimed forever after its first stage.
func HeldBy(t ticket.Ticket, held bool) (Claim, bool) {
	if !held {
		return Claim{}, false
	}
	winner, ok := Oldest(t.Comments)
	if !ok {
		return Claim{}, false
	}
	c, err := Parse(winner.Body)
	if err != nil {
		return Claim{}, false
	}
	return c, true
}

// Attempts counts the claims a role has recorded on this ticket since the last
// hand-back.
//
// COMMENTS ARE SORTED RATHER THAN TRUSTED IN FILE ORDER. The platform does
// return them in order today, but the whole meaning of this count depends on
// which side of a hand-back marker each claim falls, and that is too much to
// rest on an undocumented guarantee — the arbitration above sorts for the same
// reason.
func Attempts(t ticket.Ticket, role string) int {
	ordered := make([]ticket.Comment, len(t.Comments))
	copy(ordered, t.Comments)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].CreatedAt.Equal(ordered[j].CreatedAt) {
			return ordered[i].CreatedAt.Before(ordered[j].CreatedAt)
		}
		return ordered[i].ID < ordered[j].ID
	})

	n := 0
	for _, cm := range ordered {
		if isReset(cm.Body) {
			n = 0
			continue
		}
		if !IsClaim(cm.Body) {
			continue
		}
		if c, err := Parse(cm.Body); err == nil && c.Role == role {
			n++
		}
	}
	return n
}

func isReset(body string) bool {
	for _, m := range resetsAttempts {
		if strings.Contains(body, m) {
			return true
		}
	}
	return false
}

// StaleAfter is how long a claim may stand before it is treated as abandoned
// rather than as an attempt spent.
//
// Comfortably longer than any real attempt — a lease and an iteration budget
// both expire well inside it — so this only ever forgives claims whose process
// is gone.
const StaleAfter = 45 * time.Minute

// PruneStale drops claims whose process is gone, so they do not count as
// attempts spent. It returns a copy; the ticket handed in is not modified.
//
// A CLAIM IS WRITTEN WHEN WORK STARTS, and nothing closes it if the process dies
// mid-attempt. The ceiling counts claims, so an interrupted run silently costs
// the ticket an attempt — and three of them make it permanently unclaimable,
// with nothing on the board to say why. Measured and self-inflicted: restarting
// blacksmith three times to install fixes stranded a ticket in ready_for_dev
// that no developer would take.
//
// KEPT OUT OF Attempts ON PURPOSE. That function answers "how many claims are
// recorded", a counting question its tests pin precisely; this answers "which
// claims are still real", a liveness question, and belongs where the ceiling is
// applied.
//
// now is passed in rather than read from the clock so the boundary is testable
// at all — the behaviour here is entirely about where a timestamp falls relative
// to the present.
func PruneStale(t ticket.Ticket, now time.Time) ticket.Ticket {
	kept := make([]ticket.Comment, 0, len(t.Comments))
	for _, cm := range t.Comments {
		if IsClaim(cm.Body) && isRealTime(cm.CreatedAt) && now.Sub(cm.CreatedAt) > StaleAfter {
			continue
		}
		kept = append(kept, cm)
	}
	t.Comments = kept
	return t
}

// isRealTime reports whether a timestamp carries liveness information at all.
//
// A TIMESTAMP THAT IS NOT A REAL TIME MUST NOT BE READ AS "VERY OLD". Claims
// come back from the platform with real times; a zero or epoch value means the
// field was never populated, and treating that as abandoned would forgive every
// claim ever made — which is the failure mode that hurts, since it un-holds work
// that is actively being worked.
func isRealTime(ts time.Time) bool {
	return ts.Year() >= 2000
}
