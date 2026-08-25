package window

import (
	"bufio"
	"encoding/json"
	"fmt"
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

// ReadActivity reads the newest state of every task the transcripts mention.
//
// A MISSING OR UNREADABLE CORPUS IS NOT AN ERROR. This is a window, and the
// board is still worth drawing without it, so every failure here degrades to an
// empty map rather than refusing to render.
func ReadActivity(dir string, now time.Time) map[string]Activity {
	out := map[string]Activity{}
	for _, r := range recentRecords(dir) {
		if r.TaskID == "" {
			continue
		}
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
	return out
}

// recentRecords reads today's and yesterday's transcripts in order.
//
// Two files, because anything older is not "happening now" and reading the whole
// corpus would turn a two-second refresh into a scan of every run this host has
// ever done.
func recentRecords(dir string) []transcript.Record {
	if dir == "" || dir == config.TranscriptOff {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(dir, "transcripts-*.jsonl"))
	sort.Strings(files) // the names are dated, so lexical order is chronological
	if len(files) > 2 {
		files = files[len(files)-2:]
	}
	var out []transcript.Record
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		// A turn record carries the whole prompt, which is far past the scanner's
		// default 64KB line cap. Without this the reader stops at the first big
		// record and every ticket after it reads as idle.
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for sc.Scan() {
			var r transcript.Record
			if json.Unmarshal(sc.Bytes(), &r) != nil {
				continue // a half-written final line is normal on a live file
			}
			out = append(out, r)
		}
		fh.Close()
	}
	return out
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
	case strings.Contains(d, "git clone"), strings.Contains(d, "git diff"):
		return "reading the repository"
	case r.Tool == "verify":
		return "verifying: " + clip(d, 48)
	case r.Tool == "pipeline":
		return "pipeline: " + clip(d, 48)
	case r.Tool == "ticket-comment":
		return "commenting on the ticket"
	case r.Tool != "":
		return r.Tool + ": " + clip(d, 40)
	}
	return clip(d, 56)
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
