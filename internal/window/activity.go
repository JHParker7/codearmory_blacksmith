package window

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/transcript"
)

// Activity is what an agent is doing on one ticket, as of the newest record the
// transcripts hold for it.
//
// THE BOARD ALONE CANNOT ANSWER "IS IT MOVING". A column says where a ticket got
// to, not whether anything is happening to it, so a claimed ticket being edited
// and a claimed ticket whose host died look identical on the board. The
// transcripts are the only place the difference is written down.
type Activity struct {
	Role string
	// What is the readable verb: "thinking", "pushing a branch", "finished: ok".
	What string
	At   time.Time
	// Turns counts model calls, and rolls up: see RollUp.
	Turns      int
	Finished   bool
	LastStatus string

	// Working counts how many DESCENDANTS are live, and is zero on the ticket
	// actually doing the work.
	//
	// The first roll-up copied a live descendant's role and state onto every
	// ancestor, so a root, its task and its section all read "dev-agent ·
	// editing" — a board on which everything appeared to be working while one
	// thing was. A parent's honest state is how many of its children are busy,
	// not an impersonation of the busiest.
	Working int
}

// StaleAfter is how long without a record before work is presumed not to be in
// flight.
//
// TEN MINUTES, because a developer turn against a large model on a busy host has
// been measured at over four minutes, and a threshold under that flickers a
// working ticket to idle between its own turns. It is deliberately generous: the
// cost of calling a dead agent live for ten minutes is a stale row, and the cost
// of calling a live agent dead is someone intervening in work that was fine.
const StaleAfter = 10 * time.Minute

// Live reports whether this looks like work in progress rather than a finished
// task.
//
// A transcript with no outcome record is the signal the format was designed
// around: it marks a task that never finished, which from here is the same shape
// as one still running.
//
// A ticket the transcripts have never mentioned needs no special case: its zero
// clock is two thousand years stale, so the staleness arm already answers no.
func (a Activity) Live(now time.Time) bool {
	return !a.Finished && now.Sub(a.At) < StaleAfter
}

// applyActivity folds one record into the activity map.
func applyActivity(out map[string]Activity, r transcript.Record) {
	{
		a := out[r.TaskID]
		if r.Role != "" {
			a.Role = r.Role
		}
		a.At = r.At
		switch r.Kind {
		case transcript.KindTurn:
			a.Turns++
			a.What = "thinking"
			if r.Model != "" {
				a.What = "thinking · " + r.Model
			}
		case transcript.KindAction:
			a.What = DescribeAction(r)
		case transcript.KindRefusal:
			// A REFUSAL IS THE MOST INFORMATIVE RECORD ON THE BOARD and the old
			// window dropped it, so an agent looping on the same rejected edit
			// showed as "thinking" for as long as it took someone to open the
			// transcript. The code is a closed set, so it is safe to show raw.
			a.What = "refused: " + r.Tool
		case transcript.KindOutcome:
			a.Finished, a.LastStatus, a.What = true, r.Status, "finished: "+r.Status
		case transcript.KindStart:
			// A RESTART CLEARS THE OUTCOME. The same ticket claimed a second time
			// is a new attempt, and carrying the previous attempt's "finished"
			// forward would show a running agent as done.
			a.Turns, a.Finished, a.LastStatus = 0, false, ""
			a.What = "starting"
		}
		out[r.TaskID] = a
	}
}

// scanRecords streams today's and yesterday's transcripts in order, calling fn
// for each record.
//
// Two files, because anything older is not "happening now" and reading the whole
// corpus would turn a two-second refresh into a scan of every run this host has
// ever done.
//
// A CALLBACK RATHER THAN A SLICE, and the difference is not stylistic. Returning
// []Record materialised every record in both files — prompts, completions and
// all — so a caller wanting twenty lines of reasoning paid to parse and hold the
// entire corpus. Measured on a real 48MB corpus: 212ms per call, and this is
// called per refresh. Streaming lets each caller keep only what it wants.
func scanRecords(dir string, fn func(transcript.Record)) {
	if dir == "" || dir == config.TranscriptOff {
		return
	}
	files, _ := filepath.Glob(filepath.Join(dir, "transcripts-*.jsonl"))
	sort.Strings(files) // the names are dated, so lexical order is chronological
	if len(files) > 2 {
		files = files[len(files)-2:]
	}
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		tailOf(fh)
		sc := bufio.NewScanner(fh)
		// A turn record carries the whole prompt, which is far past the scanner's
		// default 64KB line cap. Without this the reader stops at the first big
		// record and every ticket after it reads as idle.
		// Sized from TailBytes deliberately: see tailOf. A token larger than this
		// does not merely fail — it ends the scan of the entire file.
		sc.Buffer(make([]byte, 0, 64*1024), TailBytes+(1<<20))
		for sc.Scan() {
			var r transcript.Record
			if json.Unmarshal(sc.Bytes(), &r) != nil {
				continue // a half-written final line is normal on a live file
			}
			fn(r)
		}
		fh.Close()
	}
}

// TailBytes is how much of each transcript file is read.
//
// THE WHOLE FILE WAS NEVER THE QUESTION. This window answers "what is happening
// now", and one busy day wrote 45MB by itself — parsing all of it measured at
// 243ms, paid on every refresh and growing without bound as the corpus does. A
// fixed tail makes the cost of a refresh independent of how long this host has
// been running, which is the property that actually matters.
//
// Eight megabytes is far more than "now" needs: the reasoning panel shows twenty
// turns and the activity feed keeps only the newest record per ticket. The
// honest cost is that a ticket whose last activity is further back than this
// shows nothing — which is the right answer, because such a ticket is not
// running.
const TailBytes = 8 << 20

// tailOf positions fh at the last TailBytes of the file.
//
// THE SEEK LANDS MID-LINE, and that is fine: the fragment before the first
// newline is the tail of a JSON object, which fails to parse and is skipped by
// the same guard that handles a half-written final line. An earlier version
// discarded it explicitly; measurement showed the two are indistinguishable, so
// the machinery went rather than being kept for a case it did not cover.
//
// What DOES have to hold is that the fragment fits the scanner's buffer — a
// token past it makes bufio.Scanner abandon the whole file, taking every later
// record with it. The fragment cannot exceed TailBytes, so the buffer is sized
// from TailBytes rather than from a constant that could drift away from it.
//
// A file shorter than the tail is read whole. The size check is not load-bearing
// — a negative seek fails and leaves the offset at 0, which happens to be right
// — but relying on a failed syscall for correct behaviour is not something to
// leave implied.
func tailOf(fh *os.File) {
	info, err := fh.Stat()
	if err != nil || info.Size() <= TailBytes {
		return
	}
	fh.Seek(info.Size()-TailBytes, io.SeekStart) //nolint:errcheck // offset 0 on failure, which reads the whole file
}

// Digest is everything the window wants from the transcripts, from ONE pass.
//
// Built together because the corpus is large and reading it is the expensive
// part: gathering the reasoning alongside the activity costs almost nothing on
// a scan that is happening anyway, where a second scan costs the same again.
type Digest struct {
	Acts     map[string]Activity
	Thoughts map[string][]Thought
}

// ReadDigest makes one pass and returns both products.
//
// ids bounds the reasoning to the tickets actually on the board — the corpus
// holds every run this host has ever done, and keeping the rest would be paying
// to remember what nothing can display.
func ReadDigest(dir string, ids []string, now time.Time) Digest {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	d := Digest{Acts: map[string]Activity{}, Thoughts: map[string][]Thought{}}

	scanRecords(dir, func(r transcript.Record) {
		if r.TaskID == "" {
			return
		}
		applyActivity(d.Acts, r)
		if want[r.TaskID] {
			if th, ok := thoughtOf(r); ok {
				list := append(d.Thoughts[r.TaskID], th)
				// Bounded AS IT GOES rather than at the end, so a ticket with ten
				// thousand turns costs twenty entries rather than ten thousand.
				if len(list) > MaxThoughtsShown {
					list = list[len(list)-MaxThoughtsShown:]
				}
				d.Thoughts[r.TaskID] = list
			}
		}
	})
	return d
}

// DescribeAction turns a recorded action into something readable at a glance.
//
// The sandbox scripts are hundreds of characters of shell; what a watcher wants
// is the verb. The order matters — the specific shapes are matched before the
// generic tool name, because "run: git clone https://…" tells you less than
// "reading the repository".
func DescribeAction(r transcript.Record) string {
	d := r.Detail
	switch {
	case strings.Contains(d, "git push"):
		return "pushing a branch"
	case strings.Contains(d, "--- lint ---"), strings.Contains(d, "HARNESS_GATE"):
		return "running the checks"
	case strings.Contains(d, "git ls-files"):
		return "surveying the repository"
	case strings.Contains(d, "===FILE "):
		return "reading files"
	case strings.Contains(d, "git clone"), strings.Contains(d, "git diff"):
		return "reading the repository"
	case r.Tool == "verify":
		return "verifying: " + clip(d, 48)
	case r.Tool == "pipeline":
		return "pipeline: " + clip(d, 48)
	case r.Tool == "ticket-comment":
		return "commenting on the ticket"
	}

	// EVERY LEASED COMMAND CARRIES THE SAME PREFIX — the prelude, then the branch
	// adoption — so without stripping it the first forty characters of every
	// action are identical and the row says nothing at all. Measured on a live
	// run: six consecutive actions all rendered as "sandbox: sh -c set -e mkdir
	// -p /tmp/.ca…".
	if rest := commandOf(d); rest != "" {
		if r.Tool != "" {
			return r.Tool + ": " + clip(rest, 40)
		}
		return clip(rest, 56)
	}
	if r.Tool != "" {
		return r.Tool + ": " + clip(d, 40)
	}
	return clip(d, 56)
}

// commandOf drops the boilerplate every leased command begins with, leaving what
// the stage actually asked for. Empty when nothing is left, so the caller can
// fall back rather than render a blank.
func commandOf(script string) string {
	var kept []string
	for _, line := range strings.Split(script, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "", t == "set -e", t == "sh -c set -e":
		case strings.HasPrefix(t, "mkdir -p"), strings.HasPrefix(t, "cd "),
			strings.HasPrefix(t, "export "):
		case strings.HasPrefix(t, "git fetch"), strings.HasPrefix(t, "git checkout"),
			strings.HasPrefix(t, "git reset"), strings.HasPrefix(t, "git clean"):
		default:
			kept = append(kept, t)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "; "))
}

// RollUp gives a ticket the activity of its descendants when it has none of its
// own.
//
// A PARENT NEVER WORKS, so it never has turns. The root of a decomposed request
// sits in "tracking" while thirteen sections beneath it are claimed, edited and
// merged, and its row read "turn 0" throughout — which looks exactly like a hung
// pipeline and was reported as one twice. The work is really happening; it is
// happening one or two levels down, filed under the ticket that is doing it.
//
// Own activity wins when there is any, because a ticket that IS working should
// show its own state rather than a summary of its children.
func RollUp(tickets []ticket.Ticket, acts map[string]Activity, now time.Time) map[string]Activity {
	kids := map[string][]string{}
	for _, t := range tickets {
		if t.ParentID != nil && *t.ParentID != "" {
			kids[*t.ParentID] = append(kids[*t.ParentID], t.ID)
		}
	}
	out := make(map[string]Activity, len(acts))
	for k, v := range acts {
		out[k] = v
	}

	seen := map[string]bool{}
	var gather func(id string) Activity
	gather = func(id string) Activity {
		if seen[id] {
			// ONE VISIT PER TICKET. A listing can carry the same child twice — two
			// projects sharing a board, or a read that raced a write — and without
			// this its turns are added to the parent once per copy, so a request
			// reports more effort than was spent. Returning the zero value rather
			// than the ticket's own activity is what makes the second copy add
			// nothing at all.
			return Activity{}
		}
		seen[id] = true
		self := out[id]
		own := self.Live(now)
		for _, c := range kids[id] {
			ca := gather(c)
			// TURNS ROLL UP; THE STATE DOES NOT. A parent never works, so summing
			// its children's effort is the useful number — but claiming their role
			// made every ancestor look busy. It reports how many are busy instead.
			self.Turns += ca.Turns
			self.Working += ca.Working
			// ONLY A TICKET WITH AN AGENT ON IT COUNTS. A child that is itself only
			// reporting "n working below" has no agent of its own, and counting it
			// too inflated every level: two busy sections read as three at the root.
			if ca.Live(now) && ca.Role != "" {
				self.Working++
			}
			// The newest descendant only sets the CLOCK, so an ancestor's age
			// reflects the work beneath it rather than when it was last touched.
			if ca.At.After(self.At) {
				self.At = ca.At
			}
		}
		// A ticket doing its own work keeps describing that; one whose children are
		// working says so plainly.
		if !own && self.Working > 0 {
			self.Role, self.Finished = "", false
			self.What = fmt.Sprintf("%d working below", self.Working)
			if self.Working == 1 {
				self.What = "1 working below"
			}
		}
		out[id] = self
		return self
	}

	for _, t := range tickets {
		if t.ParentID == nil || *t.ParentID == "" {
			gather(t.ID)
		}
	}
	return out
}
