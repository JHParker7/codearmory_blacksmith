// Package plan is the breakdown: one request becomes the units of work that
// answer it.
//
// THE MOST CONTAINED AGENT IN THE DEPARTMENT. Its whole effect on the world is a
// ticket comment, a priority from an allowlist, and the child tickets it opens.
// It writes no code, touches no branch, and runs no command.
//
// Everything in this file exists because a model's breakdown is a proposal
// rather than an answer. The prompt asks for what is wanted; the sanitisers
// below are what happens when it does not arrive, and each one is here because
// the prompt alone was measured to be insufficient.
package plan

import (
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/edit"
)

// Bounds on what a breakdown may contain.
//
// A model that emits a very long response must not turn into a very long ticket
// comment, and these caps are cheaper than trusting a token ceiling alone.
const (
	MaxLabels   = 8
	MaxSubtasks = 12

	// MaxCriteriaBeforeSplit is how much a single unit of work may ask for before
	// it is broken down again.
	//
	// THE ONLY TICKET THAT EVER MERGES IS THE SMALLEST ONE. Across a full day of
	// runs the foundation ticket — no dependencies, tightest scope — merged in
	// every configuration, while the API, interface and validation tickets merged
	// in none of them. Three criteria is roughly where the foundation ticket sat.
	MaxCriteriaBeforeSplit = 3

	MaxNeedsDetail  = 6
	MaxItemRunes    = 200
	MaxSummaryRunes = 500

	// MaxAcceptance bounds the criteria per subtask. Five is enough to state what
	// a unit of work must do; a longer list is a subtask that should have been
	// two, and it becomes a test file the developer cannot finish.
	MaxAcceptance = 5
)

// Priorities is the allowlist. Model output outside it is DISCARDED rather than
// forwarded, so the ticket keeps whatever a person set.
var Priorities = []string{"low", "medium", "high", "critical"}

// Subtask is one unit of work and what genuinely blocks it.
//
// DependsOn holds indexes into the same list, because the model has no ticket
// ids to refer to — the tickets do not exist until after it answers.
type Subtask struct {
	Title string `json:"title"`

	// File is the source file this subtask's code belongs in, and it exists to
	// keep parallel work from colliding.
	//
	// Subtasks fan out from one request and are built SIMULTANEOUSLY on separate
	// branches by agents that cannot see each other. When each writes to the
	// obvious place — one file in a single-package repository — the integrator
	// merges the first and every branch after it conflicts. Measured: the store
	// ticket and the data-structure ticket both appended to one file, the second
	// reaching conflicted sixty seconds after the first merged.
	//
	// A HINT, not a constraint the harness enforces: a ticket that genuinely must
	// touch shared code still can, and still conflicts, which is what the resolver
	// is for.
	File string `json:"file,omitempty"`

	DependsOn []int `json:"depends_on,omitempty"`

	// Acceptance is what this subtask must do, DRAWN FROM THE REQUEST.
	//
	// It exists because the stage after this one works from almost nothing: a
	// child ticket carries a one-line title and the original request, so the
	// author has to INVENT the detail — and every invention becomes an assertion
	// the developer is bound to and cannot edit. Measured across four runs: the
	// requirements that consumed whole budgets — case-insensitive matching, an
	// empty slice rather than nil, sequential ids — appear nowhere in the request.
	// They are an agent filling a blank page.
	Acceptance []string `json:"acceptance,omitempty"`

	// GroupTitle names the unit a split child came from. Set when refining, never
	// by the model, so the breakdown stays legible: without it twelve leaves all
	// hang off the original request and nothing shows which three came from one
	// piece.
	GroupTitle string `json:"-"`
}

// Triage is the whole reply.
type Triage struct {
	Summary     string    `json:"summary"`
	Priority    string    `json:"priority"`
	Labels      []string  `json:"labels"`
	Subtasks    []Subtask `json:"subtasks"`
	NeedsDetail []string  `json:"needs_detail"`
	Rationale   string    `json:"rationale"`
}

// Sanitise bounds a breakdown and validates its edges.
func Sanitise(t Triage) Triage {
	if !slices.Contains(Priorities, t.Priority) {
		// An unknown priority leaves the ticket's own value alone rather than
		// overwriting what a person chose with a guess.
		t.Priority = ""
	}
	t.Summary = clip(t.Summary, MaxSummaryRunes)
	t.Rationale = clip(t.Rationale, MaxSummaryRunes)
	t.Labels = clipList(t.Labels, MaxLabels)
	t.Subtasks = SanitiseSubtasks(t.Subtasks)
	t.NeedsDetail = clipList(t.NeedsDetail, MaxNeedsDetail)
	return t
}

// SanitiseSubtasks bounds the breakdown and validates its edges.
//
// IT FAILS OPEN. An edge that cannot be trusted is DROPPED, never replaced with
// a guess — and if the graph contains a cycle, every edge goes. That asymmetry
// is deliberate: a missing dependency costs at most a merge conflict, which the
// resolver already handles, while a wrong one stalls work that was ready and
// says on the board that it is waiting for something. One is recoverable and
// visible; the other is neither.
//
// An earlier version avoided the judgement entirely by chaining subtasks in the
// order proposed. That is worse than it looks: it serialises work with no
// relationship, so a request for two independent functions ran as four
// sequential agent cycles instead of two parallel ones.
func SanitiseSubtasks(in []Subtask) []Subtask {
	out := make([]Subtask, 0, min(len(in), MaxSubtasks))

	// remap turns an index into the MODEL's list into an index into the kept
	// list, or -1 for a subtask that was dropped.
	//
	// DROPPING SHIFTS EVERY LATER POSITION, so edges must be translated rather
	// than reused: without this a dependency on "the store" silently becomes a
	// dependency on whatever moved into slot 0.
	remap := make([]int, len(in))
	for i := range remap {
		remap[i] = -1
	}

	// A file claimed twice is worse than a file claimed once, because the whole
	// point of the assignment is that no two subtasks share one. THE FIRST CLAIM
	// WINS and later ones are dropped rather than renamed: a wrong guess at a name
	// would send an agent to a file nobody meant.
	claimed := map[string]bool{}

	for i, st := range in {
		st.Title = clip(st.Title, MaxItemRunes)
		if st.Title == "" || NotCodeWork(st.Title) {
			continue
		}
		st.Acceptance = clipList(st.Acceptance, MaxAcceptance)
		st.File = SanitiseFile(st.File, claimed)
		if st.File != "" {
			claimed[st.File] = true
		}
		if len(out) == MaxSubtasks {
			break
		}
		remap[i] = len(out)
		out = append(out, st)
	}

	// Edges resolve THROUGH the remap, so an index into a dropped subtask is
	// itself dropped rather than silently pointing at whatever now occupies that
	// position.
	for i := range out {
		kept := make([]int, 0, len(out[i].DependsOn))
		seen := map[int]bool{}
		for _, d := range out[i].DependsOn {
			if d < 0 || d >= len(remap) {
				continue
			}
			n := remap[d]
			if n == -1 || n == i || seen[n] {
				continue
			}
			seen[n] = true
			kept = append(kept, n)
		}
		out[i].DependsOn = kept
	}

	if HasCycle(out) {
		for i := range out {
			out[i].DependsOn = nil
		}
	}
	return FlattenToRoots(out)
}

// NotCodeWork reports whether a proposed subtask is one this department cannot
// build: documentation, or "write the tests".
//
// BOTH ARE ALREADY DONE BY A STAGE OF THEIR OWN, and the pipeline has nowhere to
// put a ticket asking for them. The architect writes documentation into the base
// branch before the breakdown; the author writes tests for every ticket before
// its code. A ticket asking for either is not merely redundant, it is
// UNSATISFIABLE: the author may write only test files, so a "write the README"
// ticket has no permitted action that reaches its finish condition. One held a
// large-class slot for 642 seconds, produced a test file that tests a README,
// and handed the developer a gate it could not pass.
//
// The prompt already forbids both, in capitals, and a model emitted them anyway
// — twice on the same request. THAT IS THE REASON THIS IS CODE: a rule the model
// may decline to follow is not a rule.
//
// DELIBERATELY NARROW. Dropping real work is far worse than letting one odd
// ticket through, so the match anchors on the LEADING VERB rather than searching
// the whole title: "Implement filtering and document the query parameters" is
// implementation work that merely mentions documenting, and must survive.
func NotCodeWork(title string) bool {
	words := strings.Fields(strings.ToLower(title))
	if len(words) == 0 {
		return false
	}

	// Only titles OPENING with a producing verb are candidates. Anything
	// beginning "implement", "build", "fix" is code work whatever it mentions.
	switch words[0] {
	case "create", "write", "add", "update", "produce", "document":
	default:
		return false
	}
	if words[0] == "document" {
		return true
	}

	head := words
	if len(head) > 5 {
		head = head[:5] // the object of the verb, not the rest of the sentence
	}
	clean := func(s string) string { return strings.Trim(s, ".,:;()") }

	for i, w := range head {
		switch clean(w) {
		case "readme", "readme.md", "changelog", "docs", "documentation":
			return true

		// PLURAL ONLY. "tests" is the deliverable; a singular "test" is almost
		// always a modifier on something real — a test harness, a test command, a
		// test endpoint — and dropping those would be silent lost scope.
		case "tests":
			return true
		case "test":
			if i+1 < len(head) {
				switch clean(head[i+1]) {
				case "suite", "coverage":
					return true
				}
			}
		}
	}
	return false
}

// FlattenToRoots re-points every dependency at the foundations a subtask
// ultimately rests on, collapsing chains into fan-out.
//
// The prompt already asks for dependencies that genuinely block, and models
// ignore it: asked to break down an API, one returned store → handlers →
// validation → filtering → tests → README, a six-deep chain in which nothing can
// run beside anything else. SEQUENCING IS THE NATURAL WAY TO DESCRIBE WORK, so
// it is what gets emitted, and the cost is paid twice over — no parallelism at
// all, and a single failure anywhere strands every ticket behind it. Measured:
// five minutes of work, thirteen minutes of deadlock.
//
// So the graph is NORMALISED rather than trusted. Each subtask keeps only the
// ROOTS of its ancestor closure, which leaves the foundations first and
// everything else able to start together.
//
// The trade is deliberate: a subtask whose real prerequisite is mid-chain may
// now start before that work has merged, and fail. That is RECOVERABLE — it is
// retried, and the attempt is cheap now a sandbox is held per ticket. A chain is
// not recoverable: one failure silently strands the rest.
func FlattenToRoots(in []Subtask) []Subtask {
	roots := func(start int) []int {
		seen := map[int]bool{start: true}
		stack := append([]int(nil), in[start].DependsOn...)
		var out []int

		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[n] {
				continue
			}
			seen[n] = true
			if len(in[n].DependsOn) == 0 {
				out = append(out, n)
				continue
			}
			stack = append(stack, in[n].DependsOn...)
		}
		sort.Ints(out)
		return out
	}

	for i := range in {
		if len(in[i].DependsOn) == 0 {
			continue
		}
		in[i].DependsOn = roots(i)
	}
	return in
}

// HasCycle reports whether the breakdown contains a cycle, which would leave
// every subtask in it waiting on another and none of them workable.
func HasCycle(in []Subtask) bool {
	const (
		unvisited = iota
		open
		closed
	)
	state := make([]int, len(in))

	var walk func(int) bool
	walk = func(i int) bool {
		switch state[i] {
		case open:
			return true
		case closed:
			return false
		}
		state[i] = open
		for _, d := range in[i].DependsOn {
			if d < 0 || d >= len(in) {
				continue
			}
			if walk(d) {
				return true
			}
		}
		state[i] = closed
		return false
	}

	for i := range in {
		if walk(i) {
			return true
		}
	}
	return false
}

// SanitiseFile validates the source file a subtask claims.
//
// THE MODEL SUPPLIES THIS STRING and it becomes a path an agent is pointed at,
// so anything that escapes the tree, names a test file, or repeats a claim is
// dropped rather than repaired — a wrong guess at a name would send an agent to
// a file nobody meant.
func SanitiseFile(p string, claimed map[string]bool) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = strings.TrimPrefix(p, "./")

	if len(p) > MaxItemRunes || path.IsAbs(p) || strings.HasPrefix(p, "~") {
		return ""
	}
	if p != path.Clean(p) || strings.HasPrefix(p, "../") || p == ".." {
		return ""
	}
	// A TEST FILE IS NOT THIS STAGE'S TO ASSIGN: those are written by the author,
	// and a developer pointed at one would be told it may not write there.
	if edit.IsTestFile(p) || claimed[p] {
		return ""
	}
	return p
}

// NeedsSplit reports whether a unit of work asks for more than one ticket's
// worth.
func NeedsSplit(st Subtask) bool { return len(st.Acceptance) > MaxCriteriaBeforeSplit }

// Task is one unit of work and the slices of specification that describe it.
//
// TWO LEVELS, KEPT APART. The outer is a TASK: a unit of work, developed on one
// branch. The inner are its SECTIONS: slices of that task's specification, each
// written by its own agent so each slice actually gets written. Flattening them
// was the mistake — it turned a specification device into a work-partitioning
// one, and gave every slice its own branch, developer, merge and place in a
// dependency queue.
type Task struct {
	Task     Subtask
	Sections []Subtask
}

// AcceptSplit decides whether a proposed split is worth taking.
//
// A SPLIT INTO ONE IS NOT A SPLIT, and anything past three is the model ignoring
// the brief rather than finding structure. Refusing leaves the unit exactly as
// it was: this pass may improve a breakdown and must never lose one.
func AcceptSplit(children []Subtask) bool {
	return len(children) >= 2 && len(children) <= 3
}

func clipList(in []string, max int) []string {
	out := make([]string, 0, min(len(in), max))
	for _, s := range in {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		out = append(out, clip(s, MaxItemRunes))
		if len(out) == max {
			break
		}
	}
	return out
}

func clip(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return strings.TrimSpace(s)
	}
	return string(r[:max]) + "…"
}
