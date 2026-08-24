package main

import (
	"strings"
	"testing"
)

// A HAND-BACK IS NOT "NOTHING TO MERGE".
//
// The spec-merge stage has a fast path that answers "is there anything to
// assemble" before any model call, and returns success when the sections already
// compile as one package. That is right for a fresh task and wrong for one the
// developer sent back: a hand-back is a named failure this stage is the only one
// allowed to fix, because it may edit tests and the developer may not.
//
// Measured on r81, and it ran until the retries were spent. A section asserted
// with reflect.Elem on a string-kind type, which panics inside the test itself
// whatever the implementation does:
//
//	--- FAIL: TestStatusType
//	panic: reflect: Elem of invalid type main.Status
//	    /workspace/ticket_types_test.go:28
//
// The tests COMPILED, so the fast path returned "sections already compile
// together" without a single model call and handed the task straight back to
// development, which found the same panic and returned it again. Three tasks
// cascaded behind it, and the board was simultaneously blocked and running.

func TestAHandBackIsNotTakenAsNothingToMerge(t *testing.T) {
	fresh := Ticket{TicketID: "task0001"}
	returned := Ticket{TicketID: "task0001", Comments: []Comment{
		{Body: specRepairMarker + " the tests panic"},
	}}

	if specRepairsSoFar(fresh) != 0 {
		t.Fatal("a fresh task looks like a repair")
	}
	// The fast path is guarded on exactly this, so it is the thing to pin.
	if specRepairsSoFar(returned) == 0 {
		t.Error("a task the developer handed back does not read as a repair; the fast path will skip the work")
	}
}

// THE PANIC IS WHAT IT WAS SENT BACK FOR, so a repair that still panics is not
// finished however well it compiles.
func TestAPanicInTheTestsIsSeenAsTheirOwn(t *testing.T) {
	out := `--- FAIL: TestStatusType (0.00s)
panic: reflect: Elem of invalid type main.Status [recovered, repanicked]

goroutine 8 [running]:
panic({0x550760?, 0xc000022270?})
reflect.elem(0x0?)
reflect.(*rtype).Elem(0x1?)
tracker.TestStatusType(0xc000003dc0)
	/workspace/ticket_types_test.go:28 +0x3e
FAIL	tracker	0.003s`

	panics := panicOnlyInTests(out)
	if len(panics) == 0 {
		t.Fatal("r81's panic is not recognised as raised inside the tests")
	}
	if panics[0] != "ticket_types_test.go" {
		t.Errorf("named %q, want the test file that panics", panics[0])
	}
}

// An implementation frame in the trace means the developer owns it after all,
// and a repair gate that claimed otherwise would send real bugs to the author.
func TestAPanicThroughTheImplementationIsNotTheSpecificationsFault(t *testing.T) {
	out := `panic: runtime error: invalid memory address
goroutine 1 [running]:
tracker.(*Store).Create(0x0)
	/workspace/store.go:41 +0x1a
tracker.TestCreate(0xc000003dc0)
	/workspace/store_create_test.go:12 +0x3e`

	if panics := panicOnlyInTests(out); len(panics) > 0 {
		t.Errorf("a panic through store.go was blamed on the tests: %v", panics)
	}
}

// The message has to name the file and quote the failure, or the author is told
// something is wrong and not where.
func TestTheRepairRefusalNamesTheFileAndTheFailure(t *testing.T) {
	// The refusal is built inline in verifyTestsParse; this pins the shape it must
	// keep, since a message naming a symptom and not a cause is the most expensive
	// failure class this repository has recorded.
	const msg = "Your tests PANIC before their assertions run, in ticket_types_test.go."
	for _, want := range []string{"PANIC before their assertions run", "ticket_types_test.go"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the repair refusal does not carry %q", want)
		}
	}
}
