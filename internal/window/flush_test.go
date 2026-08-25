package window

import (
	"os"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// openPTY returns a connected master/slave terminal pair.
//
// Done by hand because the fix under test is about a REAL terminal's input
// queue, and a pipe or a plain file has none — a test using one would pass
// whether or not anything was flushed.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()

	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pty available here: %v", err)
	}
	t.Cleanup(func() { m.Close() })

	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Skipf("cannot unlock the pty: %v", err)
	}
	n, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Skipf("cannot name the pty: %v", err)
	}

	s, err := os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("cannot open the pty slave: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return m, s
}

// queued is how many bytes are waiting to be read.
//
// ASKED DIRECTLY RATHER THAN BY READING. A read blocks when the queue is empty,
// which is the very outcome being tested — the first version of this test hung
// on exactly that. TIOCINQ answers immediately either way.
func queued(t *testing.T, f *os.File) int {
	t.Helper()
	n, err := unix.IoctlGetInt(int(f.Fd()), unix.TIOCINQ)
	if err != nil {
		t.Skipf("cannot count queued bytes here: %v", err)
	}
	return n
}

// waitForQueued gives the line discipline a moment to deliver.
func waitForQueued(t *testing.T, f *os.File) int {
	t.Helper()
	for i := 0; i < 50; i++ {
		if n := queued(t, f); n > 0 {
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 0
}

// THE QUEUED BYTES ARE DISCARDED.
//
// This is what breaks the loop reported as "it keeps happening". A window that
// ended badly can leave bytes unread on the terminal — a ^C among them becomes
// SIGINT the moment the next window starts, which cancels it before it draws,
// which leaves the terminal the same way again.
func TestFlushingATerminalDiscardsWhatIsQueued(t *testing.T) {
	master, slave := openPTY(t)

	// Something typed before the window opened, including the byte that becomes
	// SIGINT.
	if _, err := master.Write([]byte("qqq\x03\r")); err != nil {
		t.Fatalf("writing to the pty: %v", err)
	}
	if n := waitForQueued(t, slave); n == 0 {
		t.Skip("the pty never queued the write; nothing to flush")
	}

	if err := flushInput(slave); err != nil {
		t.Fatalf("flushInput: %v", err)
	}
	if n := queued(t, slave); n != 0 {
		t.Fatalf("%d bytes still queued after flushing; they reach the next window "+
			"and the ^C among them cancels it before it draws", n)
	}
}

// AND IT LEAVES THE TERMINAL USABLE. Flushing discards what is queued, not the
// terminal itself — anything typed afterwards must still arrive.
func TestFlushingDoesNotBreakTheTerminal(t *testing.T) {
	master, slave := openPTY(t)

	if err := flushInput(slave); err != nil {
		t.Fatalf("flushInput: %v", err)
	}
	if _, err := master.Write([]byte("x\r")); err != nil {
		t.Fatalf("writing to the pty: %v", err)
	}
	if n := waitForQueued(t, slave); n == 0 {
		t.Fatal("nothing arrived after the flush; the terminal was left unusable")
	}
}

// NOWHERE BUT A TERMINAL HAS AN INPUT QUEUE, and asking a plain file to flush
// one is an error the caller would then have to explain away.
func TestFlushingSomethingThatIsNotATerminal(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "notatty")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })

	if err := flushInput(f); err != nil {
		t.Fatalf("flushInput on a plain file = %v, want nil: there is nothing to "+
			"flush, which is not a failure", err)
	}
}
