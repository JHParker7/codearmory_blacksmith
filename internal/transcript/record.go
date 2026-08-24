// Package transcript records what every agent did, and why.
//
// TRANSCRIPTS ARE WORTHLESS RETROACTIVELY. A task that ran before capture
// existed cannot be recovered, which is why this lands before any agent runs
// rather than after.
//
// The unit written to disk is a RECORD, not a whole transcript: one line per
// event, appended the moment it happens, all sharing a transcript id. A
// transcript is then a group-by over records. The alternative — buffering a
// whole task and writing it at the end — loses everything for any task that
// crashes, times out or is killed by a shutdown, which is precisely the
// population whose transcripts are most interesting.
package transcript

import (
	"time"

	"github.com/code-armory-app/blacksmith/internal/model"
)

// Kind distinguishes the event types on one transcript.
type Kind string

const (
	// KindStart opens a transcript: what the task is and who is doing it.
	KindStart Kind = "start"

	// KindTurn is one model call — input messages, completion, usage, timing.
	KindTurn Kind = "turn"

	// KindAction is something the agent did to the world: a command in the
	// sandbox, a ticket comment, a push.
	KindAction Kind = "action"

	// KindOutcome closes a transcript. ITS ABSENCE IS ITSELF A SIGNAL: it marks a
	// task that never finished, and backfilling one would erase that.
	KindOutcome Kind = "outcome"

	// KindRefusal is an action the harness REJECTED — a no-op edit, a write to a
	// test file, an unparseable reply.
	//
	// Its absence was a real hole. Only sandbox commands and ticket comments were
	// ever recorded, so an attempt that died having made twenty rejected actions
	// left seven records, none of them the rejections. Four consecutive runs
	// failed the same way and each destroyed the evidence needed to explain it:
	// the transcript showed only that the agent stopped, never what it kept
	// trying. WHAT THE AGENT ATTEMPTED AND WHY IT WAS REFUSED is the single most
	// useful thing to know about a failed attempt, and it was the one thing not
	// written down.
	KindRefusal Kind = "refusal"
)

// Refusal codes.
//
// A CLOSED SET, because these become a metric label and an unbounded label is a
// cardinality bug. Anything unrecognised becomes RefusalOther, and a growing
// "other" means it is time to add a code — not to let free text through.
const (
	RefusalNoopEdit        = "noop_edit"        // the edit would not change the file
	RefusalTestFile        = "test_file"        // the developer may not write tests
	RefusalUnparseable     = "unparseable"      // the reply was not a valid action
	RefusalUnknownTool     = "unknown_tool"     // no such action
	RefusalMissingFile     = "missing_file"     // a read of a path the repository does not have
	RefusalRepeatAction    = "repeat_action"    // the identical action, again
	RefusalStaleRead       = "stale_read"       // a re-read that returned nothing new
	RefusalSpecGate        = "spec_gate"        // the specification was sent back
	RefusalSearchMiss      = "search_miss"      // the search text did not match the file
	RefusalPrematureFinish = "premature_finish" // finishing before the tests passed
	RefusalOther           = "other"
)

// refusalCodes is the closed set, for the one function allowed to widen it.
var refusalCodes = map[string]bool{
	RefusalNoopEdit: true, RefusalTestFile: true, RefusalUnparseable: true,
	RefusalUnknownTool: true, RefusalMissingFile: true, RefusalRepeatAction: true,
	RefusalStaleRead: true, RefusalSpecGate: true, RefusalSearchMiss: true,
	RefusalPrematureFinish: true, RefusalOther: true,
}

// RefusalCode maps a code onto the closed set, folding anything unknown to
// RefusalOther.
//
// HERE RATHER THAN AT EACH CALL SITE, because the guarantee is only worth
// anything if it holds everywhere. A caller that passed its own string straight
// through would put unbounded text on a metric label, and the label would look
// fine until the cardinality bill arrived.
func RefusalCode(kind string) string {
	if refusalCodes[kind] {
		return kind
	}
	return RefusalOther
}

// Record is one line of the transcript log.
type Record struct {
	TranscriptID string    `json:"transcript_id"`
	Seq          int64     `json:"seq"`
	Kind         Kind      `json:"kind"`
	At           time.Time `json:"at"`

	// Host is carried on EVERY record because several agent hosts may attach to
	// one platform. Without it, "dev-agent" on three machines is one identity for
	// three actors and a bad patch cannot be traced to the box that made it.
	Host string `json:"host"`
	Role string `json:"role,omitempty"`

	// TaskID is the ticket this work belongs to — the join key back to the
	// platform's own record of what happened.
	TaskID string `json:"task_id,omitempty"`

	// Turn fields.
	Class      model.Class     `json:"class,omitempty"`
	Model      string          `json:"model,omitempty"`
	Endpoint   string          `json:"endpoint,omitempty"`
	Messages   []model.Message `json:"messages,omitempty"`
	Completion string          `json:"completion,omitempty"`

	// Reasoning is why the model did what it did, as it stated at the time.
	//
	// Its absence was a hole of the same shape as the missing refusals above, and
	// worse: an attempt that spent 41 refusals re-applying an edit that changed
	// nothing leaves a transcript showing the loop and never the cause. Behaviour
	// alone is not enough to fix a stuck agent — inferring from it produced two
	// changes that measurably made things worse before the reasoning was read and
	// named the real problem in one turn.
	Reasoning string `json:"reasoning,omitempty"`

	PromptTokens     int   `json:"prompt_tokens,omitempty"`
	CompletionTokens int   `json:"completion_tokens,omitempty"`
	QueuedMS         int64 `json:"queued_ms,omitempty"`
	LatencyMS        int64 `json:"latency_ms,omitempty"`

	// Action and refusal fields. On a refusal, Tool carries the closed-set code
	// and Detail the target and free text.
	Tool   string `json:"tool,omitempty"`
	Detail string `json:"detail,omitempty"`

	// Outcome field.
	Status string `json:"status,omitempty"`

	// Error is set on any kind. A FAILED TURN IS SIGNAL, not noise, so it is
	// recorded rather than dropped.
	Error string `json:"error,omitempty"`
}
