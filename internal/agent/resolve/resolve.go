// Package resolve settles a merge conflict — the one agent that writes to the
// integration branch.
//
// CONFLICTS ARE NOT AN EDGE CASE HERE, THEY ARE THE NORMAL CONSEQUENCE OF THE
// SHAPE. Every agent branch is cut from the integration branch and no agent can
// see any other, so two tickets touching one file produce two branches that each
// pass alone and cannot both merge. At four concurrent developers that happens
// constantly, and without something to resolve them the integration branch
// stalls on the second ticket and never accumulates enough to promote.
//
// THIS IS THE MOST DANGEROUS AGENT IN THE DEPARTMENT, and the constraints are
// not decoration:
//
//   - It may only rewrite files GIT ITSELF reports as conflicted. A resolution
//     that edits anything else is refused, because "resolve this conflict" must
//     not become "change whatever you like on the branch everything merges into".
//   - The gates run on the RESOLVED tree and a failure means nothing is pushed. A
//     resolution that compiles is not a resolution that is right, and this is the
//     only check between a plausible-looking merge and a broken integration
//     branch.
//   - ONE ATTEMPT, then it is a person's. A conflict is two changes disagreeing
//     about intent; a model that got it wrong once has no more information the
//     second time, and looping would spend sandboxes to arrive somewhere worse.
//   - It never force-pushes and never touches the base branch.
package resolve

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/edit"
	"github.com/code-armory-app/blacksmith/internal/forge"
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/queue"
	"github.com/code-armory-app/blacksmith/internal/record"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/transcript"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// ResolvedMarker records a conflict a model reconciled.
//
// It sits ALONGSIDE the merge marker rather than replacing it: how a merge
// happened matters when reading the history back.
const ResolvedMarker = "**Merge conflict resolved automatically.**"

// FailedMarker records an attempt that did not work, so the ticket is not tried
// again.
//
// A TICKET CARRYING THIS IS BLOCKED, NOT DONE. Every failure path here once
// reported success, whose destination for this stage is done — so a conflict no
// agent could settle was filed as finished work: the branch was never merged,
// nothing said so on the board, and a run could report six tickets done with an
// integration branch that had not moved. The intent behind the success was "do
// not retry", and the vocabulary for that is BLOCKED, which puts the ticket in
// front of a person.
const FailedMarker = "**Could not resolve the conflict automatically.**"

// NoConflictMarker records a conflict that had already gone away.
//
// DELIBERATELY NOT THE FAILURE MARKER: nothing failed, and recording it as a
// spent attempt would bar the resolver from this ticket for good — so if the
// branch conflicted again, nothing would ever look at it a second time.
const NoConflictMarker = "**No longer conflicting.**"

// MaxFileForModel bounds one conflicted file in the prompt.
const MaxFileForModel = 12000

// Attempted reports whether this stage has already had its one try.
func Attempted(t ticket.Ticket) bool {
	for _, c := range t.Comments {
		if strings.Contains(c.Body, ResolvedMarker) || strings.Contains(c.Body, FailedMarker) {
			return true
		}
	}
	return false
}

// Sandbox runs a command in isolation.
type Sandbox interface {
	Run(ctx context.Context, rec forge.Recorder, spec forge.Spec) (forge.Result, error)
}

// Store is the board operations this stage needs.
type Store interface {
	AddComment(ctx context.Context, id, body string) (ticket.Comment, error)
}

// Gateway is the model call this needs.
type Gateway interface {
	Chat(ctx context.Context, class model.Class, req model.ChatRequest) (model.ChatResult, error)
}

// Agent resolves one conflict.
type Agent struct {
	gw      Gateway
	sandbox Sandbox
	store   Store
	class   model.Class
	repo    config.Repo
	branch  string
}

// New builds the resolver.
func New(gw Gateway, sandbox Sandbox, store Store, class model.Class, repo config.Repo, branch string) *Agent {
	if branch == "" {
		branch = config.DefaultIntegrationBranch
	}
	return &Agent{gw: gw, sandbox: sandbox, store: store, class: class, repo: repo, branch: branch}
}

func (a *Agent) Role() string       { return workflow.RoleResolve }
func (a *Agent) Class() model.Class { return a.class }

// Wants takes tickets the integrator could not merge mechanically.
//
// ONLY A CONFLICT, never an integration failure. Those look similar on a ticket
// and are opposite problems: a conflict is a disagreement to settle, while a tree
// that merges cleanly and fails its tests is a change that is wrong in
// combination, which no amount of reconciling text will fix.
//
// ONE ATTEMPT PER CONFLICT, and this stays a comment check rather than a column
// one: the conflicted column cannot express it, because a resolver that tried and
// failed leaves the ticket exactly where it found it — so the column alone would
// hand it back the same work forever.
func (a *Agent) Wants(t ticket.Ticket) bool { return !Attempted(t) }

// Handle reproduces the conflict, asks for a resolution once, and pushes only if
// the gates pass on the result.
func (a *Agent) Handle(ctx context.Context, t ticket.Ticket) (workflow.Outcome, string, error) {
	if a.repo.URL == "" {
		return a.fail(ctx, t, "no repository is configured",
			errors.New("resolver: no repository configured"))
	}
	branch := record.BranchOf(t)
	if branch == "" {
		return a.fail(ctx, t, "no branch is recorded on this ticket, so there is nothing to merge",
			fmt.Errorf("resolver: no branch recorded on %s", t.ID))
	}

	// 1. Reproduce the conflict and read the conflicted files, MARKERS AND ALL:
	// the markers are the only record of which side each hunk came from.
	conflicted, err := a.readConflict(ctx, branch)
	if err != nil {
		return a.fail(ctx, t, "the conflict could not be reproduced: "+err.Error(),
			fmt.Errorf("resolver: read conflict: %w", err))
	}

	if len(conflicted) == 0 {
		// It merges now — something else moved. Handed BACK to the integrator
		// rather than merged here, which would duplicate that stage.
		a.comment(ctx, t, NoConflictMarker+
			"\n\nThe branch no longer conflicts; returning it to the integrator to merge normally.")
		return workflow.OutcomeReturned, "no longer conflicting", nil
	}

	// 2. ONE MODEL CALL. Not a loop — see the note at the top of this file.
	resolved, err := a.propose(ctx, t, branch, conflicted)
	if err != nil {
		a.comment(ctx, t, fmt.Sprintf(
			"%s\n\nThe model did not return a usable resolution: %v\n\nConflicted files:\n```\n%s\n```",
			FailedMarker, err, strings.Join(sortedKeys(conflicted), "\n")))
		return workflow.OutcomeBlocked, "no usable resolution", nil
	}

	// 3. Apply, verify, push — and only push if the gates pass on the result.
	res, err := a.sandbox.Run(ctx, recorderFrom(ctx), forge.Spec{
		Image:       a.repo.Image,
		Command:     []string{"sh", "-c", a.ApplyScript(branch, resolved)},
		RunnerClass: a.repo.RunnerClass,
		TimeoutSecs: a.repo.TimeoutSecs,
		SecretRefs:  a.secretRefs(),
	})
	if err != nil {
		return a.fail(ctx, t, "the resolution could not be applied: "+err.Error(),
			fmt.Errorf("resolver: apply: %w", err))
	}
	if !res.OK() {
		a.comment(ctx, t, fmt.Sprintf(
			"%s\n\nA resolution was produced for %s but the gates FAIL on the result, so nothing "+
				"was pushed.\n\n```\n%s\n```",
			FailedMarker, strings.Join(sortedKeys(conflicted), ", "), clip(res.Stdout+res.Stderr, 1800)))
		return workflow.OutcomeBlocked, "resolution did not pass", nil
	}

	a.comment(ctx, t, fmt.Sprintf(
		"%s\n\n`%s` → `%s`. Reconciled: %s.\n\nThe gates passed on the resolved tree.\n\n"+
			"Read the merge commit before trusting it: a resolution that compiles is not necessarily "+
			"the one either change intended.",
		ResolvedMarker, branch, a.branch, strings.Join(sortedKeys(conflicted), ", ")))
	return workflow.OutcomeSuccess, "resolved " + branch, nil
}

// fail ends the stage LOUDLY. Every failure path goes through it.
//
// These paths once returned an error and posted nothing, and the cost was worse
// than the missing message: the attempt is recorded by the COMMENT, so a resolver
// that failed without commenting stayed eligible, was picked up on the next poll,
// failed the same way, and burned a sandbox every time — while the ticket showed
// a claim and no explanation. Silence was not just unhelpful, it was a loop.
//
// THE MARKER GOES ON EVERY FAILURE, including ones that might be transient. A
// conflict already means a person is needed; this stage is a best-effort attempt
// to save them the work, and when it cannot, the honest state is "a person is
// needed, and here is what was tried".
func (a *Agent) fail(ctx context.Context, t ticket.Ticket, why string, err error) (workflow.Outcome, string, error) {
	a.comment(ctx, t, fmt.Sprintf("%s\n\n%s\n\nThis branch still needs a person to merge it.",
		FailedMarker, why))
	return workflow.OutcomeFailed, "", err
}

func (a *Agent) readConflict(ctx context.Context, branch string) (map[string]string, error) {
	res, err := a.sandbox.Run(ctx, recorderFrom(ctx), forge.Spec{
		Image:       a.repo.Image,
		Command:     []string{"sh", "-c", a.ConflictScript(branch)},
		RunnerClass: a.repo.RunnerClass,
		TimeoutSecs: a.repo.TimeoutSecs,
		SecretRefs:  a.secretRefs(),
	})
	if err != nil {
		return nil, err
	}
	return DecodeFiles(res.Stdout), nil
}

// ConflictScript reproduces the merge and emits the conflicted files.
func (a *Agent) ConflictScript(branch string) string {
	base := a.repo.Branch
	if base == "" {
		base = "main"
	}
	return forge.Preamble + fmt.Sprintf(`URL="${GIT_CLONE_URL:-%s}"
git clone --filter=blob:none "$URL" repo >/dev/null 2>&1
cd repo
git config user.name resolver
git config user.email resolver@blacksmith.invalid
git checkout -q %q 2>/dev/null || git checkout -q %q
# THE "|| true" IS LOAD-BEARING: the preamble sets -e, and this merge is EXPECTED
# to fail. Without it the script exits at the very conflict it was sent to read,
# produces no output, and an unresolved conflict is reported as no conflict.
git merge --no-ff --no-edit origin/%s >/dev/null 2>&1 || true
for f in $(git diff --name-only --diff-filter=U); do
  echo "===FILE $f"
  base64 -w0 < "$f"
  echo
done
`, a.repo.URL, a.branch, base, branch)
}

// ApplyScript redoes the merge, writes the resolution, verifies and pushes.
//
// THE MERGE IS REDONE rather than carried between sandboxes, because a sandbox is
// ephemeral: there is no working tree to keep. Redoing it is also the check that
// the conflict is still the same one — if the branch moved underneath, the write
// lands on a different merge and the gates are what catch it.
func (a *Agent) ApplyScript(branch string, resolved map[string]string) string {
	base := a.repo.Branch
	if base == "" {
		base = "main"
	}

	s := forge.Preamble + fmt.Sprintf(`URL="${GIT_CLONE_URL:-%s}"
git clone --filter=blob:none "$URL" repo >/dev/null 2>&1
cd repo
git config user.name resolver
git config user.email resolver@blacksmith.invalid
git checkout -q %q 2>/dev/null || git checkout -q %q
git merge --no-ff --no-edit origin/%s || true
`, a.repo.URL, a.branch, base, branch)

	// BASE64, exactly as the developer writes files: this is model-authored
	// content and it must never be read by the shell. Sorted, so the same
	// resolution produces the same script.
	for _, path := range sortedKeys(resolved) {
		s += fmt.Sprintf("\nmkdir -p \"$(dirname %q)\"\necho %q | base64 -d > %q\n",
			path, base64.StdEncoding.EncodeToString([]byte(resolved[path])), path)
	}

	// REFUSE TO CONTINUE IF ANYTHING IS STILL CONFLICTED. A partial write would
	// otherwise be committed with markers in it.
	s += `
if [ -n "$(git diff --name-only --diff-filter=U)" ]; then
  echo "still conflicted after writing the resolution"; exit 1
fi
git add -A
`
	if a.repo.FormatCommand != "" {
		s += "{ " + a.repo.FormatCommand + " ; } || true\ngit add -A\n"
	}
	s += fmt.Sprintf("git commit -q --no-edit || git commit -q -m %q\n",
		"merge: resolve conflicts merging "+branch)

	for _, cmd := range []string{a.repo.TestCommand, a.repo.CriticalCommand} {
		if cmd != "" {
			s += "{ " + cmd + " ; }\n"
		}
	}
	s += fmt.Sprintf("git push origin %s\n", a.branch)
	return s
}

// propose asks for the resolution and validates that it stays inside the
// conflict.
func (a *Agent) propose(ctx context.Context, t ticket.Ticket, branch string, conflicted map[string]string) (map[string]string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Ticket: %s\n\n%s\n\nBranch %s does not merge into %s. Each file below is a "+
		"merge result with git's conflict markers left in.\n\n",
		t.Title, clip(t.Description, 1200), branch, a.branch)
	for _, path := range sortedKeys(conflicted) {
		fmt.Fprintf(&b, "===== %s =====\n%s\n\n", path, clip(conflicted[path], MaxFileForModel))
	}

	res, err := a.gw.Chat(ctx, a.class, model.ChatRequest{
		Messages: []model.Message{
			{Role: "system", Content: SystemPrompt},
			// The conflicted content goes in a USER turn. It is branch content, and
			// on a compromised branch that is attacker-authored text.
			{Role: "user", Content: b.String()},
		},
		Temperature: 0,
		MaxTokens:   4000 + model.ThinkingHeadroom,
		Priority:    queue.ParsePriority(t.Priority),
		// A TOOL, NOT A HAND-WRITTEN ENVELOPE. This reply carries whole source
		// files, and a Go file is made of the characters JSON cares about most:
		// quotes, backslashes, newlines, backticks. Asking a model to escape all of
		// them inside a string it is also authoring is asking for the failure that
		// was measured — a decode error that threw away a resolution and blocked a
		// ticket which had already passed its tests and its security review.
		Tools:  Tools(),
		Schema: &model.ReplySchema{Name: "resolution", Schema: ResolutionSchema()},
	})
	if err != nil {
		return nil, err
	}

	// Prefer the tool call; fall back to content for a backend that ignores tools.
	if len(res.Calls) > 0 {
		return ValidateResolution(res.Calls[0].Arguments, conflicted)
	}
	return ValidateResolution(res.Content, conflicted)
}

// ValidateResolution decodes a proposed resolution and refuses anything outside
// the conflict.
//
// SEPARATED FROM THE MODEL CALL so the rules can be tested without one — these
// are the constraints that keep "resolve this" from becoming "write what you
// like to the branch everything merges into".
func ValidateResolution(raw string, conflicted map[string]string) (map[string]string, error) {
	var out struct {
		Files []struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		} `json:"files"`
	}
	if err := decodeObject(raw, &out); err != nil {
		return nil, err
	}
	if len(out.Files) == 0 {
		return nil, errors.New("no files in the resolution")
	}

	resolved := map[string]string{}
	for _, f := range out.Files {
		path := strings.TrimSpace(f.Path)
		if _, ok := conflicted[path]; !ok {
			return nil, fmt.Errorf("resolution touches %q, which git did not report as conflicted", path)
		}
		if strings.Contains(f.Content, "<<<<<<<") || strings.Contains(f.Content, ">>>>>>>") {
			// A "resolution" with markers still in it compiles in no language, and
			// means the model COPIED rather than decided.
			return nil, fmt.Errorf("%s still contains conflict markers", path)
		}
		resolved[path] = edit.WithTrailingNewline(f.Content)
	}
	for path := range conflicted {
		if _, ok := resolved[path]; !ok {
			return nil, fmt.Errorf("%s was left unresolved", path)
		}
	}
	return resolved, nil
}

// DecodeFiles reads the file blocks the sandbox emits.
//
// BASE64, because the payload is source code with conflict markers in it: any
// delimiter this department has used has eventually turned up inside the content
// it was meant to delimit.
func DecodeFiles(out string) map[string]string {
	files := map[string]string{}
	lines := strings.Split(out, "\n")

	for i := 0; i < len(lines); i++ {
		name, ok := strings.CutPrefix(lines[i], "===FILE ")
		if !ok || i+1 >= len(lines) {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[i+1]))
		if err != nil {
			continue
		}
		files[strings.TrimSpace(name)] = string(raw)
		i++
	}
	return files
}

// Tools describes the one action this stage may take.
func Tools() []model.Tool {
	return []model.Tool{{
		Name:        "resolve_conflicts",
		Description: "Return every conflicted file with its complete resolved contents and no conflict markers.",
		Parameters:  ResolutionSchema(),
	}}
}

// ResolutionSchema is shared by the tool and the structured-output fallback, so
// the two cannot describe different shapes.
func ResolutionSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"files": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type":        "string",
							"description": "Repository-relative path, exactly as given.",
						},
						"content": map[string]any{
							"type":        "string",
							"description": "The complete resolved file, with every conflict marker removed.",
						},
					},
					"required":             []string{"path", "content"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"files"},
		"additionalProperties": false,
	}
}

// SystemPrompt states the licence and its limits.
const SystemPrompt = `You resolve git merge conflicts. You are given files containing git conflict markers (<<<<<<<, =======, >>>>>>>).

Call resolve_conflicts with every file you were given.

Rules:
- Return EVERY file you were given, with its complete resolved contents.
- Return NO other files.
- Remove every conflict marker. A file containing one is not resolved.
- KEEP BOTH CHANGES where they are independent — two functions added to one file are not a disagreement, they are two additions, and dropping either loses work someone asked for.
- Where they genuinely conflict, prefer the change that matches the ticket above.
- Change nothing beyond what the conflict requires.`

// decodeObject pulls the JSON object out of a model reply and decodes it,
// tolerating the fences and preamble models add despite being asked not to.
//
// A REPLY THAT ALREADY IS A JSON OBJECT IS NEVER FENCE-EXTRACTED. Taking the
// first fence is right when a model wrapped its answer in a code block and
// CATASTROPHIC when the answer itself contains one — and file contents routinely
// do. Leading brace is the reliable signal: nothing needs unwrapping, so nothing
// is unwrapped.
func decodeObject(raw string, into any) error {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "{") {
		if fenced := extractFenced(s); fenced != "" {
			s = fenced
		}
	}
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start == -1 || end == -1 || end < start {
		return errors.New("no JSON object in the model output")
	}
	return model.DecodeJSON(s[start:end+1], into)
}

// extractFenced returns the contents of the first fenced block, or "".
func extractFenced(s string) string {
	_, rest, ok := strings.Cut(s, "```")
	if !ok {
		return ""
	}
	// A fence may name a language on the same line.
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[nl+1:]
	}
	body, _, ok := strings.Cut(rest, "```")
	if !ok {
		return ""
	}
	return strings.TrimSpace(body)
}

func (a *Agent) secretRefs() map[string]string {
	if a.repo.SecretRef == "" {
		return nil
	}
	return map[string]string{"GIT_CLONE_URL": a.repo.SecretRef}
}

func (a *Agent) comment(ctx context.Context, t ticket.Ticket, body string) {
	if a.store == nil {
		return
	}
	if _, err := a.store.AddComment(ctx, t.ID, body); err != nil {
		recorderFrom(ctx).Action(ctx, "ticket-comment", t.ID, err)
		slog.ErrorContext(ctx, "could not write the stage's comment; its work is invisible on the ticket",
			"ticket_id", t.ID, "error", err)
	}
}

func recorderFrom(ctx context.Context) *transcript.Recorder {
	r, _ := ctx.Value(recorderKey{}).(*transcript.Recorder)
	return r
}

type recorderKey struct{}

// WithRecorder attaches a transcript to a context.
func WithRecorder(ctx context.Context, r *transcript.Recorder) context.Context {
	return context.WithValue(ctx, recorderKey{}, r)
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
