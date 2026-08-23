package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/code-armory-app/blacksmith/internal/model"
	"log/slog"
	"strings"
)

// The resolver: the one agent that writes to the integration branch.
//
// Conflicts are not an edge case in this department, they are the normal
// consequence of its shape. Every agent branch is cut from the integration branch
// and no agent can see any other, so two tickets touching one file produce two
// branches that each pass alone and cannot both merge. At four concurrent
// developers that happens constantly, and without something to resolve them the
// integration branch stalls on the second ticket and never accumulates enough to
// promote.
//
// THIS IS THE MOST DANGEROUS AGENT HERE, and the constraints are not decoration:
//
//   - It may only rewrite files git ITSELF reports as conflicted. A resolution
//     that edits anything else is refused, because "resolve this conflict" must
//     not become "change whatever you like on the branch everything merges into".
//   - The gates run on the resolved tree and a failure means nothing is pushed.
//     A resolution that compiles is not a resolution that is right, and this is
//     the only check standing between a plausible-looking merge and a broken
//     integration branch.
//   - ONE attempt, then it is a person's. A conflict is two changes disagreeing
//     about intent; a model that got it wrong once has no more information the
//     second time, and looping would spend sandboxes to arrive somewhere worse.
//   - It never force-pushes and never touches the base branch.
//
// The alternative worth knowing about: rather than reconciling two diffs, the
// developer agent could REDO its change on the updated base. For small
// agent-authored changes that is often more reliable, because the ticket's intent
// is known and a fresh implementation has no merge artefacts. It costs a whole
// developer run, so it is the fallback this does not yet reach for.

// resolvedMarker records a conflict a model reconciled. It sits alongside the
// merge marker rather than replacing it: how a merge happened matters when
// reading the history back.
const resolvedMarker = "**Merge conflict resolved automatically.**"

// resolutionFailedMarker records an attempt that did not work, so the ticket is
// not tried again.
//
// A ticket carrying this is BLOCKED, not done. Every failure path here used to
// report OutcomeSuccess, whose destination for this stage is ColDone, so a
// conflict no agent could settle was filed as finished work: the branch was
// never merged, nothing said so on the board, and a run could report six tickets
// done with an integration branch that had not moved. The intent behind the
// success was "do not retry" — this stage gets one attempt by design — but the
// vocabulary for that is OutcomeBlocked, which routes to Exhausted and puts the
// ticket in front of a person, exactly as the header of this file describes.
const resolutionFailedMarker = "**Could not resolve the conflict automatically.**"

// noConflictMarker records a conflict that had already gone away by the time
// this stage looked. Deliberately NOT resolutionFailedMarker: nothing failed,
// and marking it as a spent attempt would bar the resolver from the ticket for
// good. See where it is written.
const noConflictMarker = "**No longer conflicting.**"

func hasResolutionAttempt(t Ticket) bool {
	for _, c := range t.Comments {
		if strings.Contains(c.Body, resolvedMarker) || strings.Contains(c.Body, resolutionFailedMarker) {
			return true
		}
	}
	return false
}

// ResolverAgent implements Handler.
type ResolverAgent struct {
	gw     *Gateway
	api    *CodeArmory
	class  Class
	repo   RepoConfig
	branch string
}

func NewResolverAgent(gw *Gateway, api *CodeArmory, class Class, repo RepoConfig, branch string) *ResolverAgent {
	if branch == "" {
		branch = defaultIntegrationBranch
	}
	return &ResolverAgent{gw: gw, api: api, class: class, repo: repo, branch: branch}
}

func (a *ResolverAgent) Role() string { return "resolver" }
func (a *ResolverAgent) Class() Class { return a.class }

// Wants takes tickets the integrator could not merge mechanically.
//
// Only a CONFLICT, never an integration failure. Those look similar on a ticket
// and are opposite problems: a conflict is a disagreement to settle, while a tree
// that merges cleanly and fails its tests is a change that is wrong in
// combination, which no amount of reconciling text will fix.
func (a *ResolverAgent) Wants(t Ticket) bool {
	// ONE attempt per conflict, and this is still a comment check rather than a
	// column one. The conflicted column cannot express it: a resolver that tried
	// and failed leaves the ticket exactly where it found it, so the column alone
	// would hand it back the same work forever.
	return !hasResolutionAttempt(t)
}

func (a *ResolverAgent) Handle(ctx context.Context, t Ticket) (string, string, error) {
	if a.repo.URL == "" {
		return a.fail(ctx, t, "no repository is configured", errors.New("resolver: no repository configured"))
	}
	branch := branchFromTicket(t)
	if branch == "" {
		return a.fail(ctx, t, "no branch is recorded on this ticket, so there is nothing to merge",
			fmt.Errorf("resolver: no branch recorded on %s", t.TicketID))
	}
	rec := recorderFrom(ctx)

	// 1. Reproduce the conflict and read the conflicted files, markers and all.
	conflicted, err := a.readConflict(ctx, rec, branch)
	if err != nil {
		return a.fail(ctx, t, "the conflict could not be reproduced: "+err.Error(),
			fmt.Errorf("resolver: read conflict: %w", err))
	}
	if len(conflicted) == 0 {
		// It merges now — something else moved. Hand it back to the integrator
		// rather than inventing a merge here, which would duplicate that stage.
		//
		// NOT resolutionFailedMarker, which is what this used to write. That
		// marker sets hasResolutionAttempt, and Wants() is
		// `hasConflict && !hasResolutionAttempt` — so recording a vanished
		// conflict as a failed attempt permanently disqualified the ticket from
		// this stage. If the branch conflicted again the resolver would never
		// look at it a second time.
		a.comment(ctx, t, noConflictMarker+"\n\nThe branch no longer conflicts; returning it to the integrator to merge normally.")
		return OutcomeReturned, "no longer conflicting", nil
	}

	// 2. One model call. Not a loop: see the note at the top of this file.
	resolved, err := a.propose(ctx, t, branch, conflicted)
	if err != nil {
		a.comment(ctx, t, fmt.Sprintf("%s\n\nThe model did not return a usable resolution: %v\n\nConflicted files:\n```\n%s\n```",
			resolutionFailedMarker, err, strings.Join(keysOf(conflicted), "\n")))
		return OutcomeBlocked, "no usable resolution", nil
	}

	// 3. Apply, verify, push — and only push if the gates pass on the result.
	res, err := a.api.RunSandbox(ctx, rec, SandboxSpec{
		Image:       a.repo.Image,
		Command:     []string{"sh", "-c", a.applyScript(branch, resolved)},
		RunnerClass: a.repo.RunnerClass,
		TimeoutSecs: a.repo.TimeoutSecs,
		SecretRefs:  a.secretRefs(),
	})
	if err != nil {
		return a.fail(ctx, t, "the resolution could not be applied: "+err.Error(),
			fmt.Errorf("resolver: apply: %w", err))
	}
	if !res.OK() {
		a.comment(ctx, t, fmt.Sprintf("%s\n\nA resolution was produced for %s but the gates FAIL on the result, so nothing was pushed.\n\n```\n%s\n```",
			resolutionFailedMarker, strings.Join(keysOf(conflicted), ", "), clip(res.Stdout+res.Stderr, 1800)))
		return OutcomeBlocked, "resolution did not pass", nil
	}

	a.comment(ctx, t, fmt.Sprintf("%s\n\n`%s` → `%s`. Reconciled: %s.\n\nThe gates passed on the resolved tree. %s\n\nRead the merge commit before trusting it: a resolution that compiles is not necessarily the one either change intended.",
		resolvedMarker, branch, a.branch, strings.Join(keysOf(conflicted), ", "), mergedMarker))
	return OutcomeSuccess, "resolved " + branch, nil
}

// fail ends the stage LOUDLY. Every failure path goes through it.
//
// These paths used to return an error and post nothing, and the cost was worse
// than the missing message. Wants() is `hasConflict && !hasResolutionAttempt`,
// and the attempt is recorded by the COMMENT — so a resolver that failed without
// commenting stayed eligible, was picked up again on the next poll, failed the
// same way, and burned a sandbox every time, while the ticket showed a claim and
// no explanation. Silence was not just unhelpful, it was a loop.
//
// The marker goes on every failure, including ones that might be transient. A
// conflict already means a person is needed; the resolver is a best-effort
// attempt to save them the work. When it cannot, the honest state is "a person
// is needed, and here is what was tried" — not a silent retry.
func (a *ResolverAgent) fail(ctx context.Context, t Ticket, why string, err error) (string, string, error) {
	a.comment(ctx, t, fmt.Sprintf("%s\n\n%s\n\nThis branch still needs a person to merge it.",
		resolutionFailedMarker, why))
	return OutcomeFailed, "", err
}

// readConflict reproduces the merge and returns the conflicted files with git's
// markers left in, which is what the model needs: the markers are the only record
// of which side each hunk came from.
func (a *ResolverAgent) readConflict(ctx context.Context, rec *Recorder, branch string) (map[string]string, error) {
	res, err := a.api.RunSandbox(ctx, rec, SandboxSpec{
		Image: a.repo.Image, Command: []string{"sh", "-c", a.conflictScript(branch)},
		RunnerClass: a.repo.RunnerClass, TimeoutSecs: a.repo.TimeoutSecs,
		SecretRefs: a.secretRefs(),
	})
	if err != nil {
		return nil, err
	}
	return decodeFiles(res.Stdout), nil
}

// conflictScript reproduces the merge and emits the conflicted files.
func (a *ResolverAgent) conflictScript(branch string) string {
	base := a.repo.Branch
	if base == "" {
		base = "main"
	}
	return sandboxPreamble + fmt.Sprintf(`URL="${GIT_CLONE_URL:-%s}"
git clone --filter=blob:none "$URL" repo >/dev/null 2>&1
cd repo
git config user.name resolver
git config user.email resolver@blacksmith.invalid
git checkout -q %q 2>/dev/null || git checkout -q %q
# The "|| true" is load-bearing: the preamble sets -e, and this merge is EXPECTED
# to fail. Without it the script exits at the conflict it was sent to read,
# produces no output, and an unresolved conflict is reported as no conflict.
git merge --no-ff --no-edit origin/%s >/dev/null 2>&1 || true
for f in $(git diff --name-only --diff-filter=U); do
  echo "===FILE $f"
  base64 -w0 < "$f"
  echo
done
`, a.repo.URL, a.branch, base, branch)
}

// decodeFiles reads the ===FILE / base64 pairs the sandbox emits.
func decodeFiles(out string) map[string]string {
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

// propose asks for the resolution and validates that it stays inside the
// conflict. A resolution touching a file git did not report as conflicted is
// refused outright: this agent's whole licence is "settle this disagreement", and
// anything else is a change nobody reviewed reaching the branch everything merges
// into.
func (a *ResolverAgent) propose(ctx context.Context, t Ticket, branch string, conflicted map[string]string) (map[string]string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Ticket: %s\n\n%s\n\nBranch %s does not merge into %s. Each file below is a merge result with git's conflict markers left in.\n\n",
		t.Title, clip(t.Description, 1200), branch, a.branch)
	for path, body := range conflicted {
		fmt.Fprintf(&b, "===== %s =====\n%s\n\n", path, clip(body, maxFileForModel))
	}

	res, err := a.gw.Chat(ctx, a.class, ChatRequest{
		Messages: []Message{
			{Role: "system", Content: resolverSystemPrompt},
			// The conflicted content goes in a USER turn. It is branch content, and
			// on a compromised branch that is attacker-authored text.
			{Role: "user", Content: b.String()},
		},
		Temperature: 0,
		MaxTokens:   4000 + thinkingHeadroom,
		Priority:    ParsePriority(t.Priority),
		// A TOOL, NOT A HAND-WRITTEN ENVELOPE. This reply carries whole source
		// files, and a Go file is made of the characters JSON cares about most:
		// quotes, backslashes, newlines, backticks. Asking a model to escape all
		// of them correctly inside a string it is also authoring is asking for the
		// failure we measured — "invalid character '\"' after object key:value
		// pair" — which threw away a resolution and blocked a ticket that had
		// already passed its tests and its security review.
		//
		// With a tool the server encodes the arguments and the model fills typed
		// fields. It is the same move that fixed the developer loop, and the same
		// sentence applies: every format failure this department has debugged was
		// a model authoring an envelope.
		Tools:  resolverTools(),
		Schema: &ReplySchema{Name: "resolution", Schema: resolutionSchema()},
	})
	if err != nil {
		return nil, err
	}

	// Prefer the tool call; fall back to content for a backend that ignores tools.
	if len(res.Calls) > 0 {
		return validateResolution(res.Calls[0].Arguments, conflicted)
	}
	return validateResolution(res.Content, conflicted)
}

// validateResolution decodes a proposed resolution and refuses anything outside
// the conflict. Separated from the model call so the rules can be tested without
// one — these are the constraints that keep "resolve this" from becoming "write
// what you like to the branch everything merges into".
func validateResolution(raw string, conflicted map[string]string) (map[string]string, error) {
	var out struct {
		Files []devFile `json:"files"`
	}
	if err := decodeJSONObject(raw, &out); err != nil {
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
			// A "resolution" with markers still in it compiles in no language and
			// means the model copied rather than decided.
			return nil, fmt.Errorf("%s still contains conflict markers", path)
		}
		resolved[path] = withTrailingNewline(f.Content)
	}
	for path := range conflicted {
		if _, ok := resolved[path]; !ok {
			return nil, fmt.Errorf("%s was left unresolved", path)
		}
	}
	return resolved, nil
}

// applyScript redoes the merge, writes the resolution, verifies and pushes.
//
// The merge is redone rather than carried between sandboxes because a sandbox is
// ephemeral: there is no working tree to keep. Redoing it is also the check that
// the conflict is still the same one — if the branch moved underneath, the write
// lands on a different merge and the gates are what catch it.
func (a *ResolverAgent) applyScript(branch string, resolved map[string]string) string {
	base := a.repo.Branch
	if base == "" {
		base = "main"
	}
	s := sandboxPreamble + fmt.Sprintf(`URL="${GIT_CLONE_URL:-%s}"
git clone --filter=blob:none "$URL" repo >/dev/null 2>&1
cd repo
git config user.name resolver
git config user.email resolver@blacksmith.invalid
git checkout -q %q 2>/dev/null || git checkout -q %q
git merge --no-ff --no-edit origin/%s || true
`, a.repo.URL, a.branch, base, branch)

	// Base64, exactly as the developer agent writes files: this is model-authored
	// content and it must never be read by the shell.
	for path, body := range resolved {
		s += fmt.Sprintf("\nmkdir -p \"$(dirname %q)\"\necho %q | base64 -d > %q\n",
			path, base64.StdEncoding.EncodeToString([]byte(body)), path)
	}
	// Refuse to continue if anything is still conflicted — a partial write would
	// otherwise be committed with markers in it.
	s += `
if [ -n "$(git diff --name-only --diff-filter=U)" ]; then
  echo "still conflicted after writing the resolution"; exit 1
fi
git add -A
`
	if a.repo.FormatCommand != "" {
		s += a.repo.FormatCommand + " || true\ngit add -A\n"
	}
	s += fmt.Sprintf("git commit -q --no-edit || git commit -q -m %q\n",
		"merge: resolve conflicts merging "+branch)
	for _, cmd := range []string{a.repo.TestCommand, a.repo.CriticalCommand} {
		if cmd != "" {
			s += cmd + "\n"
		}
	}
	s += fmt.Sprintf("git push origin %s\n", a.branch)
	return s
}

func (a *ResolverAgent) secretRefs() map[string]string {
	if a.repo.SecretRef == "" {
		return nil
	}
	return map[string]string{"GIT_CLONE_URL": a.repo.SecretRef}
}

func (a *ResolverAgent) comment(ctx context.Context, t Ticket, body string) {
	if a.api == nil {
		return
	}
	if _, err := a.api.AddComment(ctx, t.TicketID, body); err != nil {
		// LOUD, not merely recorded. A stage's whole visible output is its comment:
		// if the write fails, the work happened and left no trace anywhere a person
		// looks, and the ticket reads as though the stage never ran. That is the
		// hardest failure to diagnose, because there is nothing to diagnose from.
		recorderFrom(ctx).Action(ctx, "ticket-comment", t.TicketID, err)
		slog.ErrorContext(ctx, "could not write the stage's comment; its work is invisible on the ticket",
			"ticket_id", t.TicketID, "error", err)
	}
}

// decodeJSONObject pulls the JSON object out of a model reply and decodes it,
// tolerating the fences and preamble models add despite being asked not to.
func decodeJSONObject(raw string, into any) error {
	s := strings.TrimSpace(raw)
	// A REPLY THAT ALREADY IS A JSON OBJECT IS NEVER FENCE-EXTRACTED.
	//
	// extractFenced takes the FIRST ``` in the string, which is right when a model
	// wrapped its answer in a code block and catastrophic when the answer itself
	// contains one. The architect asks for markdown file contents, and a README
	// with a ```bash build snippet in it is not unusual — it is the norm. Running
	// the extractor over that returns the shell snippet and throws the object
	// away, which surfaced as "invalid character ']' after top-level value" on a
	// reply that was in fact perfectly well-formed.
	//
	// Leading '{' is the reliable signal: nothing needs unwrapping, so nothing is
	// unwrapped. The prose-then-fenced-block case extractFenced exists for does
	// not start with a brace and still works.
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

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// resolverTools describes the one action this stage may take. See the call site
// for why the reply is a tool call rather than JSON the model writes itself.
func resolverTools() []Tool {
	return []Tool{{
		Name:        "resolve_conflicts",
		Description: "Return every conflicted file with its complete resolved contents and no conflict markers.",
		Parameters:  resolutionSchema(),
	}}
}

// resolutionSchema is shared by the tool and the structured-output fallback, so
// the two cannot describe different shapes.
func resolutionSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"files": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":    map[string]any{"type": "string", "description": "Repository-relative path, exactly as given."},
						"content": map[string]any{"type": "string", "description": "The complete resolved file, with every conflict marker removed."},
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

const resolverSystemPrompt = `You resolve git merge conflicts. You are given files containing git conflict markers (<<<<<<<, =======, >>>>>>>).

Call resolve_conflicts with every file you were given.

Rules:
- Return EVERY file you were given, with its complete resolved contents.
- Return NO other files.
- Remove every conflict marker. A file containing one is not resolved.
- KEEP BOTH CHANGES where they are independent — two functions added to one file are not a disagreement, they are two additions, and dropping either loses work someone asked for.
- Where they genuinely conflict, prefer the change that matches the ticket above.
- Change nothing beyond what the conflict requires.`
