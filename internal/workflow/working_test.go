package workflow

import "testing"

func TestIsWorkingRecognisesEveryHoldColumn(t *testing.T) {
	tb := New(Options{Architect: true, Coverage: true})
	for _, st := range tb.Stages() {
		if !tb.IsWorking(st.Working) {
			t.Errorf("IsWorking(%q) = false, but %s parks held work there", st.Working, st.Role)
		}
	}
	// A QUEUE IS NOT A HOLD. A ticket in a Ready column is unheld by definition —
	// that is what makes taking it a compare-and-set rather than a lock — so
	// counting one as working would make every queued ticket look claimed.
	for _, st := range tb.Stages() {
		if tb.IsWorking(st.Ready) {
			t.Errorf("IsWorking(%q) = true, but that is %s's queue", st.Ready, st.Role)
		}
	}
	for _, c := range []string{ColDone, ColBlocked, ColTracking, "", "not-a-column"} {
		if tb.IsWorking(c) {
			t.Errorf("IsWorking(%q) = true", c)
		}
	}
}

// A stage this host does not run holds nothing on it. The coverage column is a
// real column either way, but on a host without the stage no ticket can be
// parked there, and calling it "held" would describe work nobody is doing.
func TestIsWorkingFollowsTheStagesThisHostRuns(t *testing.T) {
	if New(Options{}).IsWorking(ColCovering) {
		t.Error("IsWorking(covering) = true on a host that runs no coverage stage")
	}
	if !New(Options{Coverage: true}).IsWorking(ColCovering) {
		t.Error("IsWorking(covering) = false on a host that does run it")
	}
}
