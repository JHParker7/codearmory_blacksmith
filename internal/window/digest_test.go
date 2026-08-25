package window

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/transcript"
)

// ---- the tail ----

// A FILE SHORTER THAN THE TAIL IS READ WHOLE. Most are, and seeking must not
// cost them their first record.
func TestASmallFileIsReadFromTheStart(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeTranscript(t, dir, "2026-08-25",
		transcript.Record{Kind: transcript.KindTurn, TaskID: "first", At: now},
		transcript.Record{Kind: transcript.KindTurn, TaskID: "last", At: now})

	got := readActs(dir, now)
	if _, ok := got["first"]; !ok {
		t.Fatal("the first record of a small file was skipped")
	}
	if _, ok := got["last"]; !ok {
		t.Fatal("the last record was skipped")
	}
}

// THE COST OF A REFRESH MUST NOT GROW WITH THE CORPUS. One busy day wrote 45MB
// by itself, and parsing all of it was paid on every refresh.
func TestOnlyTheTailOfALargeFileIsRead(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	p := filepath.Join(dir, "transcripts-2026-08-25.jsonl")

	fh, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	// One old record, then enough padding to push it well past the tail.
	old, _ := json.Marshal(transcript.Record{Kind: transcript.KindTurn, TaskID: "ancient", At: now})
	fmt.Fprintf(fh, "%s\n", old)

	pad, _ := json.Marshal(transcript.Record{
		Kind: transcript.KindTurn, TaskID: "filler", At: now,
		Completion: strings.Repeat("x", 256*1024),
	})
	for written := 0; written < TailBytes+(1<<20); written += len(pad) + 1 {
		fmt.Fprintf(fh, "%s\n", pad)
	}

	recent, _ := json.Marshal(transcript.Record{Kind: transcript.KindTurn, TaskID: "recent", At: now})
	fmt.Fprintf(fh, "%s\n", recent)
	fh.Close()

	got := readActs(dir, now)
	if _, ok := got["recent"]; !ok {
		t.Fatal("the newest record was not read — the tail is what the window is for")
	}
	if _, ok := got["ancient"]; ok {
		t.Fatal("a record far outside the tail was read; the refresh still scales " +
			"with the whole corpus")
	}
}

// HALF A RECORD IS WORSE THAN NONE: it parses as nothing, or occasionally as
// something wrong. The seek lands mid-line, so that line has to go.
func TestThePartialLineAtTheSeekIsDiscarded(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	p := filepath.Join(dir, "transcripts-2026-08-25.jsonl")

	fh, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	// A record whose TAIL contains text that would parse if read as a line start.
	trap, _ := json.Marshal(transcript.Record{
		Kind: transcript.KindTurn, TaskID: "trap", At: now,
		Completion: strings.Repeat("y", 512*1024),
	})
	for written := 0; written < TailBytes+(2<<20); written += len(trap) + 1 {
		fmt.Fprintf(fh, "%s\n", trap)
	}
	good, _ := json.Marshal(transcript.Record{Kind: transcript.KindAction, TaskID: "good",
		Tool: "run", Detail: "go test", At: now})
	fmt.Fprintf(fh, "%s\n", good)
	fh.Close()

	got := readActs(dir, now)
	// The point is that the scan produced coherent records rather than garbage:
	// the final one is intact and nothing was invented.
	if got["good"].What == "" {
		t.Fatal("the final record was not read intact after the mid-line seek")
	}
	for id := range got {
		if id != "trap" && id != "good" {
			t.Fatalf("a partial line was parsed into a record for %q", id)
		}
	}
}

// ---- the digest ----

// ONE PASS, BOTH PRODUCTS. Reading the corpus is the expensive part, so
// gathering the reasoning alongside the activity costs almost nothing where a
// second scan costs the same again.
func TestTheDigestProducesActivityAndReasoningTogether(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeTranscript(t, dir, "2026-08-25",
		transcript.Record{Kind: transcript.KindTurn, TaskID: "t1", Role: "dev-agent",
			Reasoning: "the router drops the leading slash", At: now.Add(-time.Minute)},
		transcript.Record{Kind: transcript.KindAction, TaskID: "t1", Role: "dev-agent",
			Tool: "run", Detail: "git push origin x", At: now})

	d := ReadDigest(dir, []string{"t1"}, now)
	if d.Acts["t1"].What != "pushing a branch" {
		t.Errorf("activity = %q, want the newest record", d.Acts["t1"].What)
	}
	if len(d.Thoughts["t1"]) != 1 {
		t.Fatalf("got %d thoughts, want 1", len(d.Thoughts["t1"]))
	}
	if !strings.Contains(d.Thoughts["t1"][0].Prose, "leading slash") {
		t.Errorf("reasoning = %q", d.Thoughts["t1"][0].Prose)
	}
}

// THE CORPUS HOLDS EVERY RUN THIS HOST HAS EVER DONE. Keeping reasoning for
// tickets that are not on the board is paying to remember what nothing can show.
func TestTheDigestKeepsReasoningOnlyForTheBoard(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeTranscript(t, dir, "2026-08-25",
		transcript.Record{Kind: transcript.KindTurn, TaskID: "onboard", Completion: "mine", At: now},
		transcript.Record{Kind: transcript.KindTurn, TaskID: "elsewhere", Completion: "theirs", At: now})

	d := ReadDigest(dir, []string{"onboard"}, now)
	if len(d.Thoughts["elsewhere"]) != 0 {
		t.Error("reasoning was kept for a ticket that is not on the board")
	}
	if len(d.Thoughts["onboard"]) != 1 {
		t.Error("reasoning was not kept for a ticket that is")
	}
	// ACTIVITY IS STILL GATHERED FOR EVERYTHING, because the roll-up needs the
	// children of a request whether or not the filter is showing them.
	if _, ok := d.Acts["elsewhere"]; !ok {
		t.Error("activity was dropped for an off-board ticket; the roll-up needs it")
	}
}

// BOUNDED AS IT GOES, so a ticket with ten thousand turns costs twenty entries
// rather than ten thousand.
func TestTheDigestBoundsReasoningPerTicket(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	var recs []transcript.Record
	for i := 0; i < MaxThoughtsShown*3; i++ {
		recs = append(recs, transcript.Record{Kind: transcript.KindTurn, TaskID: "t1",
			Completion: fmt.Sprintf("turn %d", i), At: now.Add(time.Duration(i) * time.Second)})
	}
	writeTranscript(t, dir, "2026-08-25", recs...)

	got := ReadDigest(dir, []string{"t1"}, now).Thoughts["t1"]
	if len(got) != MaxThoughtsShown {
		t.Fatalf("got %d thoughts, want the cap of %d", len(got), MaxThoughtsShown)
	}
	want := fmt.Sprintf("turn %d", MaxThoughtsShown*3-1)
	if got[len(got)-1].Prose != want {
		t.Fatalf("last = %q, want %q — the cap must drop the OLDEST", got[len(got)-1].Prose, want)
	}
}

func TestTheDigestWithoutACorpus(t *testing.T) {
	d := ReadDigest("off", []string{"t1"}, time.Now())
	if len(d.Acts) != 0 || len(d.Thoughts) != 0 {
		t.Fatal("a disabled corpus produced records")
	}
}

// The digest and the direct read must agree, or the detail view shows something
// different from what the board's own read would have produced.
func TestTheDigestAgreesWithTheDirectRead(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeTranscript(t, dir, "2026-08-25",
		transcript.Record{Kind: transcript.KindTurn, TaskID: "t1", Role: "dev-agent",
			Completion: "I will read the store", At: now.Add(-time.Minute)},
		transcript.Record{Kind: transcript.KindRefusal, TaskID: "t1", Role: "dev-agent",
			Tool: "test_file", Detail: "store_test.go is the author's", At: now})

	direct := readThoughts(dir, "t1")
	viaDigest := ReadDigest(dir, []string{"t1"}, now).Thoughts["t1"]

	if len(direct) != len(viaDigest) {
		t.Fatalf("direct read gave %d thoughts, digest gave %d", len(direct), len(viaDigest))
	}
	for i := range direct {
		if direct[i] != viaDigest[i] {
			t.Fatalf("thought %d differs:\n direct %+v\n digest %+v", i, direct[i], viaDigest[i])
		}
	}
}

// THE FRAGMENT AT THE SEEK MUST FIT THE SCANNER, or bufio.Scanner abandons the
// whole file and every record after it is lost. The fragment cannot exceed
// TailBytes, so the buffer must not be smaller than that.
//
// Pinned as a relationship rather than as two numbers, because they were set in
// different places and drifting apart is silent: the window would simply stop
// showing recent work on any host whose transcripts had grown.
func TestTheScannerCanHoldAnythingTheTailCanProduce(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	p := filepath.Join(dir, "transcripts-2026-08-25.jsonl")

	fh, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	// One record far larger than the tail, so the seek lands deep inside it and
	// the resulting fragment is as large as a fragment can ever be.
	huge, _ := json.Marshal(transcript.Record{
		Kind: transcript.KindTurn, TaskID: "huge", At: now,
		Completion: strings.Repeat("h", TailBytes+(4<<20)),
	})
	fmt.Fprintf(fh, "%s\n", huge)

	good, _ := json.Marshal(transcript.Record{
		Kind: transcript.KindAction, TaskID: "after", Tool: "run",
		Detail: "git push origin x", At: now,
	})
	fmt.Fprintf(fh, "%s\n", good)
	fh.Close()

	got := readActs(dir, now)
	if _, ok := got["after"]; !ok {
		t.Fatal("the record after an oversized one was lost — the scanner gave up " +
			"on the whole file rather than skipping the fragment")
	}
}
