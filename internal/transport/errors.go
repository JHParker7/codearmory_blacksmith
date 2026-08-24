// Package transport is the one HTTP call this department makes, with the
// retries, the error classes and the trace context attached to it.
//
// It exists as its own package because there are THREE hosts to talk to — the
// platform through conductor, a forge on this workstation, and a ticket store on
// the sandbox plane — and every one of them needs the same treatment. The shape
// it replaces had that treatment written twice, including the credential-renewal
// dance, which is how one copy gained a fix the other did not.
package transport

import "errors"

// The error classes callers must distinguish.
//
// The claim path in particular depends on telling a LOST RACE from a real
// failure: treating a lost race as an error would make the dispatch loop noisy
// and eventually stall it.
var (
	// ErrDenied is a 401 or 403 — the account lacks the grant. Not retryable, and
	// the signal that a plane session may need renewing.
	ErrDenied = errors.New("denied")

	// ErrNotFound is a 404.
	//
	// The platform deliberately returns 404 rather than 403 for a ticket the
	// caller cannot see, so existence does not leak. For scheduling purposes
	// "denied" and "gone" are therefore the same outcome.
	ErrNotFound = errors.New("not found")

	// ErrConflict is a lost race: a 409, or a 412 refusing a conditional write
	// because somebody else got there first. Expected, not exceptional.
	ErrConflict = errors.New("conflict")

	// ErrTransient is a 5xx or a network failure — retryable.
	ErrTransient = errors.New("transient failure")
)

// Transient reports a retryable failure. Named rather than inlined because
// several poll loops ask the same question.
func Transient(err error) bool { return errors.Is(err, ErrTransient) }

// Denied reports a credential failure, which is what drives renewal.
func Denied(err error) bool { return errors.Is(err, ErrDenied) }

// Conflict reports a lost race.
func Conflict(err error) bool { return errors.Is(err, ErrConflict) }

// NotFound reports a ticket, board or lease that is gone or invisible.
func NotFound(err error) bool { return errors.Is(err, ErrNotFound) }
