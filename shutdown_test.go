package main

import (
	"sync"
	"testing"
	"time"
)

// A STAGE THAT WILL NOT RETURN MUST NOT HOLD THE PROCESS.
//
// The wait on the way out was unbounded, so one handler ignoring its context
// hung shutdown indefinitely — and the operator's next move is a harder signal,
// which kills the process in exactly the state the grace period exists to
// avoid: mid-write, with no outcome recorded and no claim closed. Worked around
// for weeks with `pkill -QUIT`.
func TestShutdownGivesUpOnWorkThatWillNotStop(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	// Never done: the stage that ignores its context.
	defer wg.Done()

	start := time.Now()
	if waitFor(&wg, 40*time.Millisecond) {
		t.Fatal("waitFor claimed work had finished when it had not")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the wait took %s; it is not bounded by the deadline it was given",
			elapsed)
	}
}

// AND IT MUST NOT CUT OFF WORK THAT IS FINISHING. A stage that stops just as the
// signal arrives still has to record its outcome and move its ticket, and
// returning before it does is what leaves the board inconsistent.
func TestShutdownWaitsForWorkThatDoesStop(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		wg.Done()
	}()

	if !waitFor(&wg, 5*time.Second) {
		t.Error("waitFor gave up on work that finished well inside its deadline")
	}
}

// THE GRACE MUST OUTLAST THE LONGEST HONEST TAIL. The post-work move runs on a
// detached context with a thirty-second timeout of its own, so anything shorter
// would cut off the very work that keeps the board consistent — and it has to
// stay inside systemd's ninety-second default, or the process is killed rather
// than choosing how it exits.
func TestTheGracePeriodBracketsTheDetachedWrites(t *testing.T) {
	if ShutdownGrace < 45*time.Second {
		t.Errorf("ShutdownGrace is %s, shorter than the 30s a finishing stage may "+
			"spend recording its outcome and moving its ticket", ShutdownGrace)
	}
	if ShutdownGrace >= 90*time.Second {
		t.Errorf("ShutdownGrace is %s, at or past systemd's default TimeoutStopSec; "+
			"the process would be killed mid-write instead of exiting", ShutdownGrace)
	}
}
