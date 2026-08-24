package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
)

// The architect: one design pass per request, committed to the base branch.
//
// WHAT IT IS FOR. Every later agent works one ticket in a sandbox of its own and
// can see no other. It knows its own subtask and, through childDescription, the
// original request — but nothing about what the other six tickets are building,
// what the pieces are called, or how they are meant to fit. That missing shared
// picture is why six agents produce six houses rather than one. The architect
// writes it down ONCE, into the repository, BEFORE the breakdown — so it is
// simply present in the tree every sandbox clones, with no plumbing to carry it
// and nothing for an agent to remember to ask for.
//
// WHY IT IS NOT A TICKET. Documentation was previously a subtask like any other
// and could not survive the pipeline: a "write the README" ticket reaching the
// spec author is unsatisfiable by construction, because that stage may write
// only *_test.go and the ticket asks for a README. No permitted action reaches
// its finish condition. One such ticket burned all 100 iterations rewriting the
// same file and then blocked, holding a large-class slot throughout. Docs are
// context, not work, and this is the stage that treats them that way.
//
// WHY IT IS NOT THE PRODUCT MANAGER. Triage and design are different jobs, and
// the PM is deliberately the most contained agent in the department — its whole
// effect on the world is one comment and a priority from an allowlist. Writing
// to the repository is a much larger blast radius, so it belongs to a stage
// whose containment can be reasoned about on its own: this one writes markdown,
// only markdown, and only once per request.
type ArchitectAgent struct {
	gw    *Gateway
	api   *CodeArmory
	class Class
	repo  RepoConfig
}

func NewArchitectAgent(gw *Gateway, api *CodeArmory, class Class, repo RepoConfig) *ArchitectAgent {
	return &ArchitectAgent{gw: gw, api: api, class: class, repo: repo}
}

func (a *ArchitectAgent) Role() string { return roleArchitect }
func (a *ArchitectAgent) Class() Class { return a.class }

// designMarker records that a request has been designed, so a re-poll of the
// same column cannot design it twice.
const designMarker = "**Design**"

func hasDesign(t Ticket) bool {
	for _, c := range t.Comments {
		if strings.Contains(c.Body, designMarker) {
			return true
		}
	}
	return false
}

// Wants skips a request that has already been designed. A child ticket is never
// designed: the architect takes only from the inbox, where children never sit.
func (a *ArchitectAgent) Wants(t Ticket) bool { return !hasDesign(t) }

// architectSystemPrompt asks for the documentation a later agent will need.
//
// It asks for a PLAN, in the present tense, of something that does not exist
// yet. A model given a repository and asked to "document it" describes the empty
// scaffold it can see, which is both useless and actively misleading to the
// agents that read it afterwards.
const architectSystemPrompt = `You are the architect for a software project. You are given a feature request, and usually the repository as it stands today.

Write the project documentation that the developers who build it will read. They work on separate parts, in isolation, and this documentation is the ONLY shared picture they get of the whole system.

If existing documentation is shown to you, UPDATE it: keep what is still true, and fold the new request into it. You write whole files, so anything you leave out is deleted — a README that comes back describing only the new feature has destroyed the rest of it. If the repository is empty, write the documentation from the request alone.

Reply with ONLY a JSON object, no prose and no code fences:
{"overview":"two or three sentences on what is being built","files":[{"path":"README.md","content":"..."},{"path":"ARCHITECTURE.md","content":"..."}]}

Rules:
- Write README.md and ARCHITECTURE.md. Nothing else, and both must be markdown.
- README.md: what the project is, how to build and run it, and what each part does.
- ARCHITECTURE.md: the pieces, the NAMES they use, how they fit, and the shape of the interfaces between them. Name concrete types, functions and endpoints, because a developer building one piece needs to know exactly what the neighbouring piece is called.
- Describe what WILL be built, in the present tense, as a specification. Do not describe the current empty repository.
- Do not write code files, tests, or build configuration. Documentation only.
- Be specific and brief. Aim for a page each.`

// architectDesign is the model's reply.
type architectDesign struct {
	Overview string `json:"overview"`
	Files    []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	} `json:"files"`
}

// Limits on what a design may commit. Model output is going into the repository
// that every subsequent agent clones, so this is the narrowest useful shape:
// markdown, a handful of files, a bounded size each.
const (
	maxDesignFiles     = 4
	maxDesignFileBytes = 24000

	// maxDesignReplyTokens has to fit TWO documents plus their JSON escaping in
	// one reply, which is a different shape from an agent that emits one action at
	// a time. At 3000 it was not enough: a design ran to the ceiling mid-document
	// and the JSON never closed, so the whole stage was lost. Typical replies here
	// measure 950–1100 tokens, so this is headroom rather than a target — and a
	// reply that still hits it has degenerated rather than run long. One did:
	// twelve kilobytes ending in "## Data Lakes / No data lakes / ## Data Streams
	// / No data streams", a repetition loop no ceiling can fix.
	maxDesignReplyTokens = 6000 + thinkingHeadroom
)

// Handle designs one request and commits the documentation to the base branch.
func (a *ArchitectAgent) Handle(ctx context.Context, t Ticket) (string, string, error) {
	if a.repo.URL == "" {
		return OutcomeFailed, "", errors.New("architect: no repository configured")
	}

	rec := recorderFrom(ctx)

	// READ THE REPOSITORY FIRST. On an empty repository this returns almost
	// nothing and the design is written from the ticket alone, which is the case
	// this stage was built for. On a repository that already has code it is the
	// difference between updating the documentation and REPLACING it: the model
	// writes whole files, so an architect that had never seen the existing README
	// would overwrite it with a description of one feature.
	//
	// Failure here is not fatal. A design written from the ticket alone is worth
	// more than no design, so the context is best-effort and its absence is noted
	// in the prompt rather than aborting the stage.
	msgs := []Message{{Role: "system", Content: architectSystemPrompt}}
	if repoCtx := a.readRepo(ctx, rec); repoCtx != "" {
		msgs = append(msgs, Message{Role: "user", Content: repoCtx})
	}
	msgs = append(msgs, Message{Role: "user", Content: renderTicket(t)})

	res, err := a.gw.Chat(ctx, a.class, ChatRequest{
		Messages: msgs,
		// NOT ZERO, UNIQUELY AMONG THESE AGENTS, and greedy decoding is why. Every
		// other stage emits one short structured action, where always taking the
		// likeliest token is exactly what is wanted. This one writes two pages of
		// prose, and a small model decoding greedily over that length falls into
		// repetition: the reply that hit the 3000-token ceiling ended "## Data
		// Lakes / No data lakes / ## Data Streams / No data streams", and raising
		// the ceiling to 6000 bought a longer version of the same loop rather than
		// a finished document. A little noise is what breaks the cycle, and design
		// prose is the one output here with no single correct wording to protect.
		Temperature: 0.3,
		MaxTokens:   maxDesignReplyTokens,
		Priority:    ParsePriority(t.Priority),
		// The SHAPE is still not left to chance. A grammar-constrained sampler
		// cannot emit an unclosed object or a stray closing brace, which is the
		// other half of what has been losing designs.
		Schema: &ReplySchema{Name: "design", Schema: designSchema()},
	})
	if err != nil {
		return OutcomeFailed, "", fmt.Errorf("design %s: %w", t.TicketID, err)
	}

	var design architectDesign
	if err := decodeJSONObject(res.Content, &design); err != nil {
		// Exhausted, not failed: the request is still worth breaking down, and
		// this stage is explicitly not a gate. Say so on the ticket rather than
		// letting a quiet stage look like a working one.
		//
		// THE REPLY ITSELF GOES ON THE TICKET. "The model did not return usable
		// JSON" is not a diagnosis — an empty reply, a refusal, a fenced block and
		// a truncated object all produce it, and they need opposite fixes. Without
		// the text there is nothing to reason from but the timing, and blacksmith's
		// own log lines do not reach the journal.
		// TRUNCATION AND MALFORMEDNESS READ THE SAME AND ARE NOT THE SAME. A reply
		// that stopped at the ceiling is a length problem; one that stopped early is
		// the model's. Saying which is the difference between a comment that can be
		// acted on and one that only records that something went wrong.
		cause, detail := "the reply is not valid JSON", "unparseable model output"
		if res.CompletionTokens >= maxDesignReplyTokens {
			cause = fmt.Sprintf("the reply was CUT OFF at the %d-token ceiling, so the JSON never closed",
				maxDesignReplyTokens)
			detail = "design reply truncated"
		}
		a.comment(ctx, t, fmt.Sprintf(
			"**Design skipped.** %s (%v), so this request goes on to scoping undocumented.\n\n"+
				"It replied with %d completion tokens; the first of them were:\n\n```\n%s\n```",
			cause, err, res.CompletionTokens, clip(strings.TrimSpace(res.Content), 800)))
		return OutcomeBlocked, detail, nil
	}

	files, rejected := sanitiseDesign(design)
	if len(files) == 0 {
		a.comment(ctx, t, "**Design skipped.** The model proposed no usable documentation file, so this request goes on to scoping undocumented.")
		return OutcomeBlocked, "no usable files", nil
	}

	sres, err := a.api.RunSandbox(ctx, rec, SandboxSpec{
		Image:       a.repo.Image,
		Command:     []string{"sh", "-c", a.commitScript(files, t)},
		RunnerClass: a.repo.RunnerClass,
		TimeoutSecs: a.repo.TimeoutSecs,
		SecretRefs:  a.secretRefs(),
	})
	if err != nil {
		return OutcomeFailed, "", fmt.Errorf("architect: sandbox: %w", err)
	}
	out := sres.Stdout + sres.Stderr

	switch {
	case strings.Contains(out, designPushedMarker):
		a.comment(ctx, t, renderDesign(design, files, rejected, res))
		return OutcomeSuccess, "designed: " + strings.Join(pathsOf(files), ", "), nil

	case strings.Contains(out, designNoChangeMarker):
		// Already identical on the branch. Not a failure and not worth a retry.
		a.comment(ctx, t, designMarker+"\n\nThe documentation on `"+a.base()+"` already matches this design; nothing was committed.")
		return OutcomeSuccess, "design already current", nil

	default:
		// The docs could not be pushed. The request still goes forward — see the
		// stage's Exhausted column — because a request nobody documented is worth
		// more than a request nobody builds.
		slog.WarnContext(ctx, "architect could not commit the design", "ticket_id", t.TicketID, "output", clip(out, 400))
		a.comment(ctx, t, "**Design skipped.** The documentation could not be committed to `"+a.base()+
			"`, so this request goes on to scoping undocumented.\n\n```\n"+clip(out, 1200)+"\n```")
		return OutcomeBlocked, "could not push the design", nil
	}
}

// sanitiseDesign enforces the limits on what may be committed, and reports what
// it dropped so the ticket can say so rather than silently shipping less.
//
// The path rules are the load-bearing part: this is model output being written
// into a repository, so a path that escapes the tree, hides in a dotfile, or is
// not documentation at all is refused rather than cleaned up into something
// plausible.
func sanitiseDesign(d architectDesign) (files []designFile, rejected []string) {
	seen := map[string]bool{}
	for _, f := range d.Files {
		p := path.Clean(strings.TrimSpace(f.Path))
		switch {
		case p == "" || p == ".":
			continue
		case path.IsAbs(p), strings.HasPrefix(p, ".."), strings.HasPrefix(p, "."):
			rejected = append(rejected, f.Path)
			continue
		case !strings.HasSuffix(strings.ToLower(p), ".md"):
			rejected = append(rejected, f.Path)
			continue
		case strings.TrimSpace(f.Content) == "":
			rejected = append(rejected, f.Path)
			continue
		case len(f.Content) > maxDesignFileBytes:
			rejected = append(rejected, f.Path+" (too large)")
			continue
		case seen[p]:
			continue
		}
		seen[p] = true
		files = append(files, designFile{Path: p, Content: f.Content})
		if len(files) == maxDesignFiles {
			break
		}
	}
	return files, rejected
}

type designFile struct {
	Path    string
	Content string
}

func pathsOf(files []designFile) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

// Markers the script prints so an outcome is read from the script's own words
// rather than from git's, which vary by version.
const (
	designPushedMarker   = "===DESIGN-PUSHED==="
	designNoChangeMarker = "===DESIGN-NO-CHANGE==="
)

// Budgets for the repository context. The serving slots carry 8192 tokens each
// (32768 split four ways) and the reply may use 3000 of them, so the whole
// prompt has to fit in roughly 4000 — call it 16000 characters, and stay well
// inside it. A design written from slightly less of the tree is a small loss; a
// request that overflows the context and returns nothing is a total one.
const (
	maxRepoContextBytes = 10000
	maxRepoContextFiles = 12
	maxContextFileBytes = 2500
)

// readRepo returns a bounded picture of the repository as it exists now:
// the file list, the documentation already written, and a sample of the source.
//
// EXISTING DOCUMENTATION COMES FIRST and is never truncated away before the
// code, because the most damaging thing this stage can do is replace a good
// README with a narrower one. The code is what lets it describe what is really
// there rather than what a ticket says; the docs are what let it UPDATE rather
// than overwrite.
//
// Best-effort by design: any failure returns "", the design is written from the
// ticket alone, and the stage carries on.
func (a *ArchitectAgent) readRepo(ctx context.Context, rec *Recorder) string {
	res, err := a.api.RunSandbox(ctx, rec, SandboxSpec{
		Image:       a.repo.Image,
		Command:     []string{"sh", "-c", a.contextScript()},
		RunnerClass: a.repo.RunnerClass,
		TimeoutSecs: a.repo.TimeoutSecs,
		SecretRefs:  a.secretRefs(),
	})
	if err != nil {
		slog.DebugContext(ctx, "architect could not read the repository; designing from the ticket alone",
			"error", err)
		return ""
	}
	out := strings.TrimSpace(res.Stdout)
	if out == "" {
		return ""
	}
	return "The repository as it stands today. Update this documentation rather than replacing it, " +
		"and describe what is really here plus what the request adds:\n\n" + clip(out, maxRepoContextBytes)
}

// contextScript prints the tree, then every markdown file, then a sample of the
// source. Ordering is the budget: clip() cuts from the end, so what matters most
// is printed first.
func (a *ArchitectAgent) contextScript() string {
	var b strings.Builder
	b.WriteString(sandboxPreamble)
	fmt.Fprintf(&b, "URL=\"${GIT_CLONE_URL:-%s}\"\n", a.repo.URL)
	b.WriteString("git clone --filter=blob:none \"$URL\" repo >/dev/null 2>&1\n")
	b.WriteString("cd repo\n")
	fmt.Fprintf(&b, "git checkout -q %s\n", shellSingleQuote(a.base()))
	b.WriteString("echo '=== FILES ==='\n")
	b.WriteString("git ls-files | head -200\n")
	// `|| true` throughout: the preamble sets -e, and grep exits 1 when a
	// repository has no markdown or no source yet — which is exactly the empty
	// baseline this stage runs against on a new project.
	b.WriteString("echo '=== DOCUMENTATION ==='\n")
	b.WriteString("for f in $(git ls-files '*.md' | head -6); do\n" +
		"  echo \"--- $f ---\"\n" +
		fmt.Sprintf("  head -c %d \"$f\" || true\n", maxContextFileBytes) +
		"  echo\n" +
		"done\n")
	b.WriteString("echo '=== SOURCE ==='\n")
	fmt.Fprintf(&b, "for f in $(git ls-files | grep -E '\\.(go|py|ts|tsx|js|rs|java|rb|c|h|cpp)$' | head -%d); do\n"+
		"  echo \"--- $f ---\"\n"+
		"  head -c %d \"$f\" || true\n"+
		"  echo\n"+
		"done\n", maxRepoContextFiles, maxContextFileBytes)
	return b.String()
}

func (a *ArchitectAgent) base() string {
	if a.repo.Branch == "" {
		return "main"
	}
	return a.repo.Branch
}

// commitScript clones the repository, writes the documentation and pushes it to
// the base branch.
//
// IT MUST CLONE FIRST. RunSandbox hands the script a bare container with a
// writable /tmp/work and nothing in it — the clone belongs to the script, which
// is why every other sandbox stage begins the same way. Assuming a checkout was
// already there produced `fatal: not in a git directory` from the first `git
// config`, on a design the model had produced perfectly well.
//
// It rebases onto the remote before pushing and NEVER force-pushes. This stage
// writes to the branch every other agent clones and the integrator merges into,
// so a lost update here would be invisible and would land in every sandbox
// afterwards. A push that cannot be made fast-forward is reported instead.
func (a *ArchitectAgent) commitScript(files []designFile, t Ticket) string {
	var b strings.Builder
	base := a.base()
	b.WriteString(sandboxPreamble)
	fmt.Fprintf(&b, "URL=\"${GIT_CLONE_URL:-%s}\"\n", a.repo.URL)
	b.WriteString("git clone --filter=blob:none \"$URL\" repo >/dev/null 2>&1\n")
	b.WriteString("cd repo\n")
	fmt.Fprintf(&b, "git checkout -q %s\n", shellSingleQuote(base))
	b.WriteString("git config user.email 'architect@blacksmith.invalid'\n")
	b.WriteString("git config user.name 'blacksmith architect'\n")
	for _, f := range files {
		fmt.Fprintf(&b, "mkdir -p \"$(dirname %q)\"\necho %q | base64 -d > %q\n",
			f.Path, base64.StdEncoding.EncodeToString([]byte(f.Content)), f.Path)
	}
	b.WriteString("git add -A\n")
	b.WriteString("if git diff --cached --quiet; then echo " + designNoChangeMarker + "; exit 0; fi\n")
	fmt.Fprintf(&b, "git commit -q -m %s\n", shellSingleQuote("docs: "+clip(t.Title, 60)))
	fmt.Fprintf(&b, "git fetch -q origin %s\n", shellSingleQuote(base))
	b.WriteString("git rebase -q FETCH_HEAD\n")
	fmt.Fprintf(&b, "git push -q origin HEAD:%s\n", shellSingleQuote(base))
	b.WriteString("echo " + designPushedMarker + "\n")
	return b.String()
}

func (a *ArchitectAgent) secretRefs() map[string]string {
	if a.repo.SecretRef == "" {
		return nil
	}
	return map[string]string{"GIT_CLONE_URL": a.repo.SecretRef}
}

func (a *ArchitectAgent) comment(ctx context.Context, t Ticket, body string) {
	if _, err := a.api.AddComment(ctx, t.TicketID, body); err != nil {
		slog.WarnContext(ctx, "could not comment the design", "ticket_id", t.TicketID, "error", err)
	}
}

// renderDesign is what a person reads on the ticket: what was written, where it
// went, and what was refused.
func renderDesign(d architectDesign, files []designFile, rejected []string, res ChatResult) string {
	var b strings.Builder
	b.WriteString(designMarker + "\n\n")
	if d.Overview != "" {
		fmt.Fprintf(&b, "%s\n\n", clip(d.Overview, 600))
	}
	fmt.Fprintf(&b, "Committed to the base branch, so every agent's sandbox clones it:\n\n")
	for _, f := range files {
		fmt.Fprintf(&b, "- `%s`\n", f.Path)
	}
	if len(rejected) > 0 {
		fmt.Fprintf(&b, "\nRefused (documentation only, markdown only): %s\n", strings.Join(clipList(rejected, 6), ", "))
	}
	fmt.Fprintf(&b, "\n<sub>%s · %d tokens</sub>", res.Model, res.CompletionTokens)
	return b.String()
}

// designSchema constrains the reply to the documented shape, so malformedness is
// prevented at the sampler rather than repaired afterwards.
func designSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"overview": map[string]any{"type": "string"},
			"files": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":    map[string]any{"type": "string"},
						"content": map[string]any{"type": "string"},
					},
					"required":             []string{"path", "content"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"overview", "files"},
		"additionalProperties": false,
	}
}
