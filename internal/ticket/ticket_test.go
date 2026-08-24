package ticket

import "testing"

// The optional fields are pointers so "not set" and "set to empty" are different
// requests, and the accessors must not lose that distinction by panicking on the
// common case.
func TestOptionalFieldsReadBackSafely(t *testing.T) {
	var bare Ticket
	if got := bare.Board(); got != "" {
		t.Errorf("Board() = %q on a ticket with none, want empty", got)
	}
	if got := bare.Parent(); got != "" {
		t.Errorf("Parent() = %q on a root ticket, want empty", got)
	}

	set := Ticket{BoardID: Ptr("b-1"), ParentID: Ptr("t-1")}
	if got := set.Board(); got != "b-1" {
		t.Errorf("Board() = %q, want b-1", got)
	}
	if got := set.Parent(); got != "t-1" {
		t.Errorf("Parent() = %q, want t-1", got)
	}
}

// EMPTY MEANS UNSET, NOT CLEAR.
//
// Every optional field here reads nil as "leave it alone" and a pointer to ""
// as "clear it". A helper that returned a pointer to empty for an empty string
// would turn "I have no board id" into "remove this ticket's board", which is a
// different request and a silent one.
func TestPtrTreatsEmptyAsUnset(t *testing.T) {
	if got := Ptr(""); got != nil {
		t.Errorf("Ptr(\"\") = %v, want nil — empty is unset, not an explicit clear", got)
	}
	got := Ptr("x")
	if got == nil || *got != "x" {
		t.Errorf("Ptr(\"x\") = %v, want a pointer to x", got)
	}
}

// A dependency whose status is empty is one this account cannot see, and an
// unseen prerequisite is UNMET: a stage cannot prove it finished, so it must not
// assume it did.
func TestAnInvisibleDependencyIsNotFinished(t *testing.T) {
	// This is a property of the type's contract rather than of any method, so the
	// test exists to state it where the next reader will find it: the zero Status
	// is meaningful and must never be defaulted to "closed" for convenience.
	var d Dependency
	if d.Status != "" {
		t.Fatalf("the zero dependency has status %q; readiness checks rely on empty meaning unknown", d.Status)
	}
}
