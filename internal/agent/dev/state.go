package dev

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/model"
)

// Step is one action the attempt took, as its own history records it.
type Step struct {
	Action string
	Detail string
}

func (s Step) String() string {
	if s.Detail == "" {
		return s.Action
	}
	return s.Action + "(" + s.Detail + ")"
}

// StepDetail summarises an action's arguments for the history line. Kept short
// on purpose: the history is replayed every turn, so it is charged for on every
// one of the few turns the attempt has.
//
// THE PATH ALONE MADE EVERY WRITE LOOK THE SAME. The trail tells the agent "do
// not repeat an action", and for a write it recorded only the file name — so
// twenty attempts at twenty different payloads and twenty attempts at the SAME
// payload were indistinguishable in its own history.
//
// The agent has no conversation memory: each turn is a fresh prompt built from
// state, so this list is the only record it has of what it already tried. A
// content fingerprint is what makes "I have written exactly this before" visible
// to it. Measured on r66: 75 writes byte-identical to one already accepted, none
// of them legible as repeats.
func StepDetail(act Action) string {
	switch act.Action {
	case ActionReadFiles:
		return clip(strings.Join(act.Paths, ", "), 160)
	case ActionWriteFiles:
		parts := make([]string, 0, len(act.Edits))
		for _, e := range act.Edits {
			sum := sha256.Sum256([]byte(e.Replace))
			parts = append(parts, fmt.Sprintf("%s lines %d-%d, %dB [%x]",
				e.Path, e.StartLine, e.EndLine, len(e.Replace), sum[:3]))
		}
		return clip(strings.Join(parts, "; "), 260)
	}
	return ""
}

// State is what one attempt knows.
//
// REBUILT INTO A PROMPT EVERY ITERATION rather than appended to a conversation.
// An ephemeral, task-scoped agent has no session to keep, and replaying a
// growing history is how a small model runs out of context halfway through.
type State struct {
	// Tree is the repository listing, Read is what has been read, Staged is what
	// has been changed but not committed.
	Tree   []string
	Read   map[string]string
	Staged map[string]string

	// Iteration and Budget are where the attempt is in its allowance. Zero budget
	// means unknown, and the denominator is left off rather than printed as
	// "OF 0".
	Iteration int
	Budget    int

	// Trail records the actions this attempt took, in order.
	//
	// IT HAS TWO READERS AND BOTH NEED IT. The person reading the ticket needs it
	// because an exhausted attempt otherwise reports only its last test output,
	// and an attempt that ended on a write has no test output at all — so the
	// ticket says "stopped after 8 iterations" and nothing else.
	//
	// The MODEL needs it because each turn is rendered from scratch, so without
	// it the model cannot see its own past actions and has no way to notice it is
	// repeating one. Observed live: an attempt whose first two actions were a
	// byte-identical read of the same three files, then a write, then a read of a
	// file it had just written — eight turns spent, nothing verified.
	Trail []Step

	// LastTest holds the VERIFICATION OUTPUT AND NOTHING ELSE; Notice holds a
	// rejection. They were one field once, and every rejection overwrote the test
	// result the model needed in order to justify finishing: the first refusal
	// erased the evidence that the code passed, and the failure comment reported
	// the refusal where the test output should have been.
	LastTest  string
	TestsPass bool
	Notice    string

	// Restart is the brief given to an agent whose memory was cleared while its
	// work was kept.
	Restart string

	// Baseline is each file's content AS IT WAS ON DISK, captured on the first
	// read and never overwritten by the agent's own writes.
	//
	// Read is NOT a substitute: it is deliberately updated with staged content so
	// the agent sees its own edits, which means by the time a second write
	// arrives there is nothing left to compare against. This exists to answer one
	// question — "is this write throwing away code that was already there" — and
	// that question needs the ORIGINAL.
	Baseline map[string]string

	// The counters that end an attempt. Each answers a different question, and
	// the two occasions they were conflated both cost whole runs.
	Refusals         int
	NoopEdits        int
	ConsecutiveReads int
	Resets           int
	LastResetAt      int

	// Missing records a path the tree listed but a read could not find, so a
	// create is not refused as an overwrite of something that is not there.
	Missing map[string]bool

	// UndoStack is the staged tree as it stood before each accepted write, newest
	// last, so undo_edit can put a file back.
	UndoStack []map[string]string

	// Repairs names the files the harness repaired on the agent's behalf.
	Repairs []string

	// History is what this agent has already done and what came of it, as a RECAP
	// IT IS SHOWN rather than a conversation it appears to have had.
	//
	// Every turn used to be a fresh two-message prompt, which made the agent a
	// stateless function: it could not remember trying something, so it re-derived
	// the same diagnosis and re-made the same edit. Read from the transcript
	// rather than inferred — turns 195 and 197 of one attempt carry byte-identical
	// prose, as do 204 and 205, and the branch shows four consecutive commits
	// removing the same duplicate methods before it deleted too much and lost
	// Update and Delete entirely. Its reasoning was correct every single time; it
	// simply had amnesia.
	//
	// Only the ACTION and a one-line outcome are kept, never the rendered state:
	// the state carries whole file contents, and replaying that per turn would
	// exhaust the slot within a few exchanges.
	History []string

	// historyBase is the last entry before any repeat marker, and historyRepeats
	// counts how many times it has arrived unchanged. Together they collapse a
	// loop into one line — see Remember.
	historyBase    string
	historyRepeats int
}

// MaxHistoryTurns bounds the record carried between turns.
//
// Enough to remember a line of attack and its consequences, not so much that the
// slot fills with old actions. Each entry is one action and one outcome line, so
// twenty of them cost far less than a single rendered state.
const MaxHistoryTurns = 20

// Remember records what the agent just did and what came of it, so the next turn
// can see it rather than re-deriving it.
func (s *State) Remember(action, outcome string) {
	if strings.TrimSpace(action) == "" {
		return
	}
	entry := strings.TrimSpace(action) + "\n" + strings.TrimSpace(outcome)

	// AN IDENTICAL STEP REPEATED IS ONE FACT, NOT SEVERAL, and writing it out
	// several times costs twice: it spends the twenty-step window on one mistake,
	// evicting the reads and edits that explain how the agent got there, and it
	// spends prompt on redundancy — r74's looping developer reached 12,565 prompt
	// tokens to emit a 200-token repeat, most of it seven copies of one refusal.
	//
	// Collapsing it also makes the loop LEGIBLE. The agent could already be told
	// not to repeat itself and did anyway; a line saying it has now done the same
	// thing seven times running is a fact about its own behaviour, which is a
	// different thing to read than an instruction.
	if n := len(s.History); n > 0 && s.historyBase == entry {
		s.historyRepeats++
		s.History[n-1] = fmt.Sprintf("%s\n(the same action, the same result, %d times in a row)",
			entry, s.historyRepeats+1)
		return
	}
	s.historyBase, s.historyRepeats = entry, 0

	s.History = append(s.History, entry)
	if n := len(s.History); n > MaxHistoryTurns {
		s.History = append([]string(nil), s.History[n-MaxHistoryTurns:]...)
	}
}

// RecentHistory is the record to show before the current state, as ONE user
// message and NEVER as assistant turns.
//
// THE ASSISTANT CHANNEL IS A FORMAT EXAMPLE, whatever it is used for. Replaying
// raw completions there taught the model to imitate the wire format, so the
// replay became descriptions instead — and it then imitated the descriptions,
// emitting `write_files: store_concurrent_test.go lines 169-169, 1B [169]` as a
// literal reply. Off-schema output is unconstrained output, and the collapse
// followed: nine of one attempt's turns ran to 9,000–17,000 characters of
// `}(i)}(i)}(i)` before the sampler was cut off.
//
// The lesson generalises past both fixes. ANYTHING PLACED IN THE ASSISTANT ROLE
// IS A DEMONSTRATION OF WHAT TO PRODUCE, so the only safe content there is a
// real reply — and a description of a reply is not one. Putting the record in
// the user channel leaves the system prompt's worked example as the single
// precedent for what an answer looks like.
func (s *State) RecentHistory() []model.Message {
	if len(s.History) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("WHAT YOU HAVE ALREADY TRIED on this ticket, oldest first, with " +
		"what came of each. Do not repeat an action that was refused, and do not " +
		"re-make an edit that already landed.")
	for i, h := range s.History {
		fmt.Fprintf(&b, "\n\n%d. %s", i+1, h)
	}
	return []model.Message{{Role: "user", Content: b.String()}}
}

// ProseOf returns what the model SAID around its action, with the JSON stripped.
//
// The syntax is what it imitates; the prose is what it reasoned. Keeping the
// second without the first is the whole point of carrying a history at all.
func ProseOf(reply string) string {
	if reply == "" {
		return ""
	}
	out := jsonBlob.ReplaceAllString(reply, " ")
	return clip(strings.Join(strings.Fields(out), " "), 400)
}

// jsonBlob matches a JSON object spanning the reply, including the fenced and
// tagged wrappers models add unprompted.
var jsonBlob = regexp.MustCompile(`(?s)` + "```" + `(?:json)?.*?` + "```" + `|<[a-z_]+>.*?</[a-z_]+>|\{.*\}`)

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
