package window

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/transcript"
)

// writeTranscript writes records to dir/transcripts-<day>.jsonl.
func writeTranscript(t *testing.T, dir, day string, recs ...transcript.Record) {
	t.Helper()
	var b strings.Builder
	for _, r := range recs {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	p := filepath.Join(dir, "transcripts-"+day+".jsonl")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

func TestReadActivityNewestRecordWins(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeTranscript(t, dir, "2026-08-25",
		transcript.Record{Kind: transcript.KindStart, TaskID: "t1", Role: "dev-agent", At: now.Add(-3 * time.Minute)},
		transcript.Record{Kind: transcript.KindTurn, TaskID: "t1", Role: "dev-agent", Model: "qwen", At: now.Add(-2 * time.Minute)},
		transcript.Record{Kind: transcript.KindAction, TaskID: "t1", Role: "dev-agent", Tool: "run", Detail: "git push origin x", At: now.Add(-time.Minute)},
	)

	got := ReadActivity(dir, now)
	a := got["t1"]
	if a.What != "pushing a branch" {
		t.Fatalf("the newest record must decide the verb, got %q", a.What)
	}
	if a.Turns != 1 {
		t.Fatalf("Turns = %d, want 1 — a turn record must count", a.Turns)
	}
	if a.Role != "dev-agent" {
		t.Fatalf("Role = %q, want dev-agent", a.Role)
	}
	if !a.Live(now) {
		t.Fatal("a task with no outcome and a recent record is live")
	}
}

func TestReadActivityTurnNamesTheModel(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeTranscript(t, dir, "2026-08-25",
		transcript.Record{Kind: transcript.KindTurn, TaskID: "t1", Model: "qwen3-coder", At: now})

	if got := ReadActivity(dir, now)["t1"].What; got != "thinking · qwen3-coder" {
		t.Fatalf("What = %q, want the model named — on a multi-model host the row "+
			"must say WHICH model is being waited on", got)
	}
}

func TestReadActivityTurnWithoutModelStillReads(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeTranscript(t, dir, "2026-08-25",
		transcript.Record{Kind: transcript.KindTurn, TaskID: "t1", At: now})

	if got := ReadActivity(dir, now)["t1"].What; got != "thinking" {
		t.Fatalf("What = %q, want %q — a missing model must not leave a dangling separator", got, "thinking")
	}
}

func TestReadActivityOutcomeFinishes(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeTranscript(t, dir, "2026-08-25",
		transcript.Record{Kind: transcript.KindTurn, TaskID: "t1", At: now.Add(-time.Minute)},
		transcript.Record{Kind: transcript.KindOutcome, TaskID: "t1", Status: "merged", At: now})

	a := ReadActivity(dir, now)["t1"]
	if !a.Finished {
		t.Fatal("an outcome record marks the task finished")
	}
	if a.LastStatus != "merged" {
		t.Fatalf("LastStatus = %q, want merged", a.LastStatus)
	}
	if a.What != "finished: merged" {
		t.Fatalf("What = %q, want the outcome named", a.What)
	}
	if a.Live(now) {
		t.Fatal("a finished task is not live however recent it is")
	}
}

// A SECOND CLAIM IS A NEW ATTEMPT. Without this the ticket keeps the previous
// attempt's outcome and a running agent renders as done.
func TestReadActivityRestartClearsTheOutcome(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeTranscript(t, dir, "2026-08-25",
		transcript.Record{Kind: transcript.KindTurn, TaskID: "t1", At: now.Add(-4 * time.Minute)},
		transcript.Record{Kind: transcript.KindOutcome, TaskID: "t1", Status: "returned", At: now.Add(-3 * time.Minute)},
		transcript.Record{Kind: transcript.KindStart, TaskID: "t1", Role: "dev-agent", At: now.Add(-time.Minute)})

	a := ReadActivity(dir, now)["t1"]
	if a.Finished {
		t.Fatal("a start after an outcome is a new attempt — it must clear Finished")
	}
	if a.LastStatus != "" {
		t.Fatalf("LastStatus = %q, want cleared by the restart", a.LastStatus)
	}
	if a.Turns != 0 {
		t.Fatalf("Turns = %d, want 0 — the new attempt counts from zero", a.Turns)
	}
	if !a.Live(now) {
		t.Fatal("the restarted attempt is live")
	}
}

// A LOOPING AGENT MUST NOT READ AS "thinking". The refusal is the record that
// says why it is stuck.
func TestReadActivityShowsRefusals(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeTranscript(t, dir, "2026-08-25",
		transcript.Record{Kind: transcript.KindTurn, TaskID: "t1", At: now.Add(-time.Minute)},
		transcript.Record{Kind: transcript.KindRefusal, TaskID: "t1", Tool: "test_file", Detail: "store_test.go", At: now})

	if got := ReadActivity(dir, now)["t1"].What; !strings.Contains(got, "test_file") {
		t.Fatalf("What = %q, want the refusal code — an agent looping on a refused "+
			"edit renders as thinking otherwise", got)
	}
}

func TestReadActivityReadsOnlyTheNewestTwoFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeTranscript(t, dir, "2026-08-20", transcript.Record{Kind: transcript.KindTurn, TaskID: "old", At: now})
	writeTranscript(t, dir, "2026-08-24", transcript.Record{Kind: transcript.KindTurn, TaskID: "yesterday", At: now})
	writeTranscript(t, dir, "2026-08-25", transcript.Record{Kind: transcript.KindTurn, TaskID: "today", At: now})

	got := ReadActivity(dir, now)
	if _, ok := got["old"]; ok {
		t.Fatal("anything older than yesterday is not happening now and must not be read")
	}
	if _, ok := got["yesterday"]; !ok {
		t.Fatal("yesterday is read: a run started last night is still running this morning")
	}
	if _, ok := got["today"]; !ok {
		t.Fatal("today must be read")
	}
}

// A TURN RECORD CARRIES THE WHOLE PROMPT. At the scanner's default 64KB cap the
// read stops at the first real turn and every ticket after it looks idle.
func TestReadActivityReadsRecordsPastTheDefaultScannerLimit(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	huge := strings.Repeat("x", 200*1024)
	writeTranscript(t, dir, "2026-08-25",
		transcript.Record{Kind: transcript.KindTurn, TaskID: "big", Completion: huge, At: now},
		transcript.Record{Kind: transcript.KindTurn, TaskID: "after", At: now})

	got := ReadActivity(dir, now)
	if _, ok := got["after"]; !ok {
		t.Fatal("a record after a 200KB one must still be read")
	}
}

// A live file's last line is often half-written. One bad line must not cost the
// rest of the corpus.
func TestReadActivitySkipsAMalformedLine(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	p := filepath.Join(dir, "transcripts-2026-08-25.jsonl")
	good, _ := json.Marshal(transcript.Record{Kind: transcript.KindTurn, TaskID: "t1", At: now})
	body := string(good) + "\n{\"kind\":\"turn\",\"task_i\n" + string(good) + "\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := ReadActivity(dir, now); got["t1"].Turns != 2 {
		t.Fatalf("Turns = %d, want 2 — a torn line must be skipped, not stop the read", got["t1"].Turns)
	}
}

func TestReadActivityWithoutACorpus(t *testing.T) {
	now := time.Now()
	for _, dir := range []string{"", config.TranscriptOff, filepath.Join(t.TempDir(), "absent")} {
		if got := ReadActivity(dir, now); len(got) != 0 {
			t.Fatalf("dir %q: want an empty map, got %d — a missing corpus is not an "+
				"error and the board is still worth drawing", dir, len(got))
		}
	}
}

func TestReadActivityIgnoresRecordsWithNoTask(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeTranscript(t, dir, "2026-08-25",
		transcript.Record{Kind: transcript.KindTurn, At: now})

	if got := ReadActivity(dir, now); len(got) != 0 {
		t.Fatalf("a record with no task id joins to no row, got %d entries", len(got))
	}
}

func TestActivityLive(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		a    Activity
		want bool
	}{
		{"recent and unfinished", Activity{At: now.Add(-time.Minute)}, true},
		{"finished", Activity{At: now, Finished: true}, false},
		{"stale", Activity{At: now.Add(-StaleAfter - time.Second)}, false},
		{"exactly at the threshold", Activity{At: now.Add(-StaleAfter)}, false},
		{"never recorded", Activity{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.a.Live(now); got != c.want {
				t.Fatalf("Live = %v, want %v", got, c.want)
			}
		})
	}
}

// A four-minute turn is normal against a large model on a busy host, so the
// threshold has to sit above it or a working ticket flickers to idle between its
// own turns.
func TestStaleAfterExceedsASlowTurn(t *testing.T) {
	if StaleAfter <= 5*time.Minute {
		t.Fatalf("StaleAfter = %s, but a developer turn has been measured over four "+
			"minutes; a shorter threshold reports live work as idle", StaleAfter)
	}
}

func TestDescribeAction(t *testing.T) {
	cases := []struct {
		name string
		rec  transcript.Record
		want string
	}{
		{"push", transcript.Record{Tool: "run", Detail: "set -e; git push origin b"}, "pushing a branch"},
		{"lint", transcript.Record{Tool: "run", Detail: "echo --- lint ---; go vet"}, "running the checks"},
		{"gate", transcript.Record{Tool: "run", Detail: "HARNESS_GATE=1 make test"}, "running the checks"},
		{"clone", transcript.Record{Tool: "run", Detail: "git clone http://git/repo"}, "reading the repository"},
		{"diff", transcript.Record{Tool: "run", Detail: "git diff --stat"}, "reading the repository"},
		{"verify", transcript.Record{Tool: "verify", Detail: "go test ./..."}, "verifying: go test ./..."},
		{"pipeline", transcript.Record{Tool: "pipeline", Detail: "run 42"}, "pipeline: run 42"},
		{"comment", transcript.Record{Tool: "ticket-comment", Detail: "anything"}, "commenting on the ticket"},
		{"other tool", transcript.Record{Tool: "edit", Detail: "store.go"}, "edit: store.go"},
		{"no tool", transcript.Record{Detail: "something happened"}, "something happened"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DescribeAction(c.rec); got != c.want {
				t.Fatalf("DescribeAction = %q, want %q", got, c.want)
			}
		})
	}
}

// THE SHAPE BEATS THE TOOL NAME. "run: set -e; cd /w && git push…" says less
// than "pushing a branch", and the whole point of the line is the verb.
func TestDescribeActionPrefersTheShapeOverTheToolName(t *testing.T) {
	r := transcript.Record{Tool: "run", Detail: "set -e; cd /w && git push origin feature"}
	if got := DescribeAction(r); got != "pushing a branch" {
		t.Fatalf("DescribeAction = %q, want the recognised shape to win over %q", got, r.Tool)
	}
}

// EVERY LEASED COMMAND CARRIES THE SAME PREFIX, so without stripping it the
// first forty characters of every action are identical and the row says nothing.
//
// Taken verbatim from a live run, where six consecutive actions all rendered as
// "sandbox: sh -c set -e mkdir -p /tmp/.ca…".
func TestDescribeActionLooksPastTheLeasedBoilerplate(t *testing.T) {
	real := "sh -c set -e\nmkdir -p /tmp/.cache\n" +
		"git fetch -q origin 'agent/53b12774' 2>/dev/null && " +
		"git checkout -q -B 'agent/53b12774' FETCH_HEAD 2>/dev/null || true\n" +
		"go build ./...\n"

	got := DescribeAction(transcript.Record{Tool: "sandbox", Detail: real})
	if strings.Contains(got, "mkdir") || strings.Contains(got, "set -e") {
		t.Fatalf("DescribeAction = %q — the boilerplate is all the reader sees", got)
	}
	if !strings.Contains(got, "go build") {
		t.Fatalf("DescribeAction = %q, want the command the stage actually ran", got)
	}
}

// THE RECOGNISED SHAPES STILL WIN over the stripped remainder, because "pushing
// a branch" is worth more than the script that does it.
func TestDescribeActionNamesTheStagesOwnWork(t *testing.T) {
	cases := []struct{ name, detail, want string }{
		{"survey", "sh -c set -e\nmkdir -p /tmp/.cache\ngit ls-files | head -n 400\n",
			"surveying the repository"},
		{"read", "sh -c set -e\nmkdir -p /tmp/.cache\nprintf '%s' '===FILE store.go'\n",
			"reading files"},
		{"push", "sh -c set -e\nmkdir -p /tmp/.cache\ngit push origin HEAD\n",
			"pushing a branch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DescribeAction(transcript.Record{Tool: "sandbox", Detail: c.detail}); got != c.want {
				t.Fatalf("DescribeAction = %q, want %q", got, c.want)
			}
		})
	}
}

// A COMMAND THAT IS NOTHING BUT BOILERPLATE still has to render as something —
// the branch adoption on its own is a real command a stage runs.
func TestDescribeActionWithNothingButBoilerplate(t *testing.T) {
	only := "sh -c set -e\nmkdir -p /tmp/.cache\ngit fetch -q origin 'agent/t1'\n"
	if got := DescribeAction(transcript.Record{Tool: "sandbox", Detail: only}); got == "" {
		t.Fatal("DescribeAction returned nothing; the row would show a blank state")
	}
}

func TestDescribeActionClipsLongDetail(t *testing.T) {
	r := transcript.Record{Tool: "edit", Detail: strings.Repeat("a", 200)}
	got := DescribeAction(r)
	if len([]rune(got)) > 60 {
		t.Fatalf("DescribeAction is %d runes; the line shares a row with a title, a "+
			"stage and an age", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("a clipped detail must say it was clipped, got %q", got)
	}
}

// ---- RollUp ----

func kid(id, parent string) ticket.Ticket {
	return ticket.Ticket{ID: id, ParentID: &parent}
}

func TestRollUpReportsChildrenWithoutImpersonatingThem(t *testing.T) {
	now := time.Now()
	ts := []ticket.Ticket{
		{ID: "root"},
		kid("a", "root"),
		kid("b", "root"),
	}
	acts := map[string]Activity{
		"a": {Role: "dev-agent", What: "editing", At: now, Turns: 4},
		"b": {Role: "dev-agent", What: "editing", At: now, Turns: 3},
	}

	got := RollUp(ts, acts, now)
	root := got["root"]
	if root.Role != "" {
		t.Fatalf("Role = %q — a parent never works, and copying a child's role made "+
			"every ancestor look busy", root.Role)
	}
	if root.What != "2 working below" {
		t.Fatalf("What = %q, want %q", root.What, "2 working below")
	}
	if root.Turns != 7 {
		t.Fatalf("Turns = %d, want 7 — effort sums even though state does not", root.Turns)
	}
	if got["a"].What != "editing" {
		t.Fatalf("the working child keeps its own state, got %q", got["a"].What)
	}
}

func TestRollUpSingularPhrasing(t *testing.T) {
	now := time.Now()
	ts := []ticket.Ticket{{ID: "root"}, kid("a", "root")}
	acts := map[string]Activity{"a": {Role: "dev-agent", What: "editing", At: now}}

	if got := RollUp(ts, acts, now)["root"].What; got != "1 working below" {
		t.Fatalf("What = %q, want %q", got, "1 working below")
	}
}

// OWN ACTIVITY WINS. A ticket that is itself working should show what IT is
// doing, not a count of its children.
func TestRollUpOwnActivityWins(t *testing.T) {
	now := time.Now()
	ts := []ticket.Ticket{{ID: "root"}, kid("a", "root")}
	acts := map[string]Activity{
		"root": {Role: "integrator", What: "merging", At: now},
		"a":    {Role: "dev-agent", What: "editing", At: now},
	}

	root := RollUp(ts, acts, now)["root"]
	if root.What != "merging" {
		t.Fatalf("What = %q, want the ticket's own state to win", root.What)
	}
	if root.Role != "integrator" {
		t.Fatalf("Role = %q, want it kept", root.Role)
	}
}

// ONLY A TICKET WITH AN AGENT ON IT COUNTS. A middle ticket that is itself only
// reporting "n working below" has no agent, and counting it too inflated every
// level: two busy sections read as three at the root.
func TestRollUpDoesNotCountReportingAncestors(t *testing.T) {
	now := time.Now()
	ts := []ticket.Ticket{
		{ID: "root"},
		kid("mid", "root"),
		kid("leaf1", "mid"),
		kid("leaf2", "mid"),
	}
	acts := map[string]Activity{
		"leaf1": {Role: "dev-agent", What: "editing", At: now},
		"leaf2": {Role: "dev-agent", What: "editing", At: now},
	}

	got := RollUp(ts, acts, now)
	if got["mid"].Working != 2 {
		t.Fatalf("mid.Working = %d, want 2", got["mid"].Working)
	}
	if got["root"].Working != 2 {
		t.Fatalf("root.Working = %d, want 2 — the middle ticket has no agent of its "+
			"own and must not be counted as a third worker", got["root"].Working)
	}
}

// The ancestor's age must reflect the work beneath it, or a busy request reads
// as untouched for as long as nobody writes to the root.
func TestRollUpTakesTheClockFromTheNewestDescendant(t *testing.T) {
	now := time.Now()
	ts := []ticket.Ticket{{ID: "root"}, kid("a", "root")}
	fresh := now.Add(-time.Second)
	acts := map[string]Activity{
		"root": {At: now.Add(-time.Hour)},
		"a":    {Role: "dev-agent", What: "editing", At: fresh},
	}

	if got := RollUp(ts, acts, now)["root"]; !got.At.Equal(fresh) {
		t.Fatalf("At = %v, want the newest descendant's %v", got.At, fresh)
	}
}

func TestRollUpIgnoresStaleChildren(t *testing.T) {
	now := time.Now()
	ts := []ticket.Ticket{{ID: "root"}, kid("a", "root")}
	acts := map[string]Activity{
		"a": {Role: "dev-agent", What: "editing", At: now.Add(-StaleAfter - time.Minute)},
	}

	if got := RollUp(ts, acts, now)["root"]; got.Working != 0 || got.What != "" {
		t.Fatalf("root = %+v, want no claim of work below: the child stopped "+
			"reporting %s ago", got, StaleAfter)
	}
}

func TestRollUpIgnoresFinishedChildren(t *testing.T) {
	now := time.Now()
	ts := []ticket.Ticket{{ID: "root"}, kid("a", "root")}
	acts := map[string]Activity{
		"a": {Role: "dev-agent", What: "finished: merged", At: now, Finished: true},
	}

	if got := RollUp(ts, acts, now)["root"]; got.Working != 0 {
		t.Fatalf("root.Working = %d, want 0 — a merged child is not working", got.Working)
	}
}

// A LISTING CAN CARRY THE SAME CHILD TWICE. Counting it twice makes a request
// report more effort than was spent, which is the number people use to judge
// whether a stage is looping.
func TestRollUpCountsADuplicatedChildOnce(t *testing.T) {
	now := time.Now()
	ts := []ticket.Ticket{{ID: "root"}, kid("a", "root"), kid("a", "root")}
	acts := map[string]Activity{"a": {Role: "dev-agent", What: "editing", At: now, Turns: 5}}

	got := RollUp(ts, acts, now)["root"]
	if got.Turns != 5 {
		t.Fatalf("root.Turns = %d, want 5 — the duplicate listing must add nothing", got.Turns)
	}
	if got.Working != 1 {
		t.Fatalf("root.Working = %d, want 1 — one agent is working, listed twice", got.Working)
	}
}

// A ticket whose OWN attempt has gone stale must not keep advertising the role
// that ran it. Otherwise a root that the architect finished with an hour ago
// reads "architect-agent · 2 working below", which names a stage that is not
// running as the thing to go and look at.
func TestRollUpClearsAStaleOwnRole(t *testing.T) {
	now := time.Now()
	ts := []ticket.Ticket{{ID: "root"}, kid("a", "root")}
	acts := map[string]Activity{
		"root": {Role: "architect-agent", What: "thinking", At: now.Add(-time.Hour)},
		"a":    {Role: "dev-agent", What: "editing", At: now},
	}

	got := RollUp(ts, acts, now)["root"]
	if got.Role != "" {
		t.Fatalf("Role = %q, want cleared: that stage stopped an hour ago and the "+
			"row would send someone to the wrong agent", got.Role)
	}
	if got.What != "1 working below" {
		t.Fatalf("What = %q, want the children reported instead", got.What)
	}
}

func TestRollUpDoesNotMutateItsInput(t *testing.T) {
	now := time.Now()
	ts := []ticket.Ticket{{ID: "root"}, kid("a", "root")}
	acts := map[string]Activity{"a": {Role: "dev-agent", What: "editing", At: now}}

	RollUp(ts, acts, now)
	if _, ok := acts["root"]; ok {
		t.Fatal("RollUp wrote into the caller's map; the transcript reading is shared")
	}
}

func TestRollUpDeepChain(t *testing.T) {
	now := time.Now()
	ts := []ticket.Ticket{{ID: "l0"}}
	for i := 1; i <= 4; i++ {
		ts = append(ts, kid(fmt.Sprintf("l%d", i), fmt.Sprintf("l%d", i-1)))
	}
	acts := map[string]Activity{"l4": {Role: "dev-agent", What: "editing", At: now, Turns: 9}}

	got := RollUp(ts, acts, now)
	if got["l0"].Turns != 9 {
		t.Fatalf("l0.Turns = %d, want the effort to reach the root through four levels", got["l0"].Turns)
	}
	if got["l0"].Working != 1 {
		t.Fatalf("l0.Working = %d, want 1", got["l0"].Working)
	}
}
