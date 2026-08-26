// Package architect writes one design pass per request, committed to the base
// branch before anything is broken down.
//
// WHAT IT IS FOR. Every later agent works one ticket in a sandbox of its own and
// can see no other. It knows its own subtask and the original request — but
// nothing about what the other six tickets are building, what the pieces are
// called, or how they are meant to fit. That missing shared picture is why six
// agents produce six houses rather than one. This writes it down ONCE, into the
// repository, BEFORE the breakdown, so it is simply present in the tree every
// sandbox clones — with no plumbing to carry it and nothing for an agent to
// remember to ask for.
//
// WHY IT IS NOT A TICKET. Documentation was previously a subtask like any other
// and could not survive the pipeline: a "write the README" ticket reaching the
// specification author is unsatisfiable by construction, because that stage may
// write only test files and the ticket asks for a README. No permitted action
// reaches its finish condition. One such ticket burned all 100 iterations
// rewriting the same file and then blocked, holding a large-class slot
// throughout. DOCS ARE CONTEXT, NOT WORK.
//
// WHY IT IS NOT THE PRODUCT MANAGER. Triage and design are different jobs, and
// that stage is deliberately the most contained agent here — its whole effect is
// one comment and a priority from an allowlist. Writing to the repository is a
// much larger blast radius, so it belongs to a stage whose containment can be
// reasoned about on its own: this one writes markdown, only markdown, and only
// once per request.
package architect

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/forge"
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/queue"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/transcript"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// Marker records that a request has been designed, so a re-poll of the same
// column cannot design it twice.
const Marker = "**Design**"

// Markers the script prints, so an outcome is read from the script's own words
// rather than from git's, which vary by version and locale.
const (
	PushedMarker   = "===DESIGN-PUSHED==="
	NoChangeMarker = "===DESIGN-NO-CHANGE==="
)

// Limits on what a design may commit.
//
// Model output is going into the repository that every subsequent agent clones,
// so this is the NARROWEST USEFUL SHAPE: markdown, a handful of files, a bounded
// size each.
const (
	MaxFiles     = 4
	MaxFileBytes = 24000

	// MaxReplyTokens has to fit TWO documents plus their JSON escaping in one
	// reply, which is a different shape from an agent emitting one action at a
	// time. At 3000 it was not enough: a design ran to the ceiling mid-document
	// and the JSON never closed, so the whole stage was lost. Typical replies
	// measure 950–1100 tokens, so this is HEADROOM rather than a target — and a
	// reply that still hits it has degenerated rather than run long.
	MaxReplyTokens = 6000 + model.ThinkingHeadroom
)

// Budgets for the repository context.
//
// A design written from slightly less of the tree is a small loss; a request
// that overflows the context and returns nothing is a total one.
const (
	MaxRepoContextBytes = 10000
	MaxRepoContextFiles = 12
	MaxContextFileBytes = 2500
)

// SystemPrompt asks for the documentation a later agent will need.
//
// It asks for a PLAN, in the present tense, of something that does not exist
// yet. A model given a repository and asked to "document it" describes the empty
// scaffold it can see, which is both useless and actively misleading to the
// agents that read it afterwards.
const SystemPrompt = `You are the architect for a software project. You are given a feature request, and usually the repository as it stands today.

Write the project documentation that the developers who build it will read. They work on separate parts, in isolation, and this documentation is the ONLY shared picture they get of the whole system.

If existing documentation is shown to you, UPDATE it: keep what is still true, and fold the new request into it. You write whole files, so anything you leave out is deleted — a README that comes back describing only the new feature has destroyed the rest of it. If the repository is empty, write the documentation from the request alone.

Reply with ONLY a JSON object, no prose and no code fences:
{"overview":"two or three sentences on what is being built","files":[{"path":"README.md","content":"..."},{"path":"ARCHITECTURE.md","content":"..."},{"path":"types/types.go","content":"package types\n\n..."}]}

Rules:
- Write README.md, ARCHITECTURE.md, and types/types.go declaring the shared types.
- types/types.go MUST begin with "package types". It goes in its own folder because
  the repository root already has a package, and two packages in one directory stop
  the whole tree building. The other stages import it.
- ARCHITECTURE.md MUST say, in one sentence, which package the code at the
  repository root belongs to — normally "package main" — and that every file there,
  tests included, declares it. Later stages write those files in isolation and
  cannot see each other: if you do not say, each one picks a different name and the
  directory ends up holding several packages, which does not compile at all.
- README.md: what the project is, how to build and run it, and what each part does.
- ARCHITECTURE.md: the pieces, the NAMES they use, how they fit, and the shape of the interfaces between them. Name concrete types, functions and endpoints, because a developer building one piece needs to know exactly what the neighbouring piece is called.
- Describe what WILL be built, in the present tense, as a specification. Do not describe the current empty repository.
- The Go file DECLARES and does not implement. Give the types, the constants, the
  errors, and the SIGNATURE of every function the pieces call across the seams
  between them. Every function body must be empty or a single panic("not implemented").
  A body that does anything is refused: the developer writes the behaviour,
  against tests you have not seen.
- Nothing you write may be a _test.go file. The tests are written by a later
  stage from this design, and writing them here replaces the stage whose job it is.
- Do not write build configuration.
- Be specific and brief. Aim for a page each.`

// Design is the model's reply.
type Design struct {
	Overview string `json:"overview"`
	Files    []File `json:"files"`
}

// File is one document.
type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Schema constrains the shape.
func Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"overview": map[string]any{
				"type":        "string",
				"description": "Two or three sentences on what is being built.",
			},
			"files": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type":        "string",
							"description": "A markdown file at the repository root, such as README.md.",
						},
						"content": map[string]any{
							"type":        "string",
							"description": "The complete file.",
						},
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

// Agent designs one request.
type Agent struct {
	gw      Gateway
	sandbox Sandbox
	store   Store
	class   model.Class
	repo    config.Repo
}

func New(gw Gateway, sandbox Sandbox, store Store, class model.Class, repo config.Repo) *Agent {
	return &Agent{gw: gw, sandbox: sandbox, store: store, class: class, repo: repo}
}

func (a *Agent) Role() string       { return workflow.RoleArchitect }
func (a *Agent) Class() model.Class { return a.class }

// Wants skips a request that has already been designed.
func (a *Agent) Wants(t ticket.Ticket) bool { return !Designed(t) }

// Designed reports whether a request already carries a design.
func Designed(t ticket.Ticket) bool {
	for _, c := range t.Comments {
		if strings.Contains(c.Body, Marker) {
			return true
		}
	}
	return false
}

// Handle designs one request and commits the documentation to the base branch.
func (a *Agent) Handle(ctx context.Context, t ticket.Ticket) (workflow.Outcome, string, error) {
	if a.repo.URL == "" {
		return workflow.OutcomeFailed, "", errors.New("architect: no repository configured")
	}

	// READ THE REPOSITORY FIRST. On an empty one this returns almost nothing and
	// the design is written from the ticket alone, which is the case this stage
	// was built for. On a repository that already has code it is the difference
	// between UPDATING the documentation and REPLACING it: the model writes whole
	// files, so an architect that had never seen the existing README would
	// overwrite it with a description of one feature.
	//
	// Failure here is NOT fatal. A design written from the ticket alone is worth
	// more than no design.
	msgs := []model.Message{{Role: "system", Content: SystemPrompt}}
	if repoCtx := a.readRepo(ctx); repoCtx != "" {
		msgs = append(msgs, model.Message{Role: "user", Content: repoCtx})
	}
	msgs = append(msgs, model.Message{Role: "user", Content: renderTicket(t)})

	res, err := a.gw.Chat(ctx, a.class, model.ChatRequest{
		Messages: msgs,
		// NOT ZERO, UNIQUELY AMONG THESE AGENTS, and greedy decoding is why. Every
		// other stage emits one short structured action, where always taking the
		// likeliest token is exactly what is wanted. This one writes two pages of
		// prose, and a small model decoding greedily over that length falls into
		// REPETITION: the reply that hit the old ceiling ended "## Data Lakes / No
		// data lakes / ## Data Streams / No data streams", and raising the ceiling
		// bought a longer version of the same loop rather than a finished document.
		// A little noise breaks the cycle, and design prose is the one output here
		// with no single correct wording to protect.
		Temperature: 0.3,
		MaxTokens:   MaxReplyTokens,
		Priority:    queue.ParsePriority(t.Priority),
		// The SHAPE is still not left to chance: a grammar-constrained sampler
		// cannot emit an unclosed object or a stray closing brace, which is the
		// other half of what has been losing designs.
		Schema: &model.ReplySchema{Name: "design", Schema: Schema()},
	})
	if err != nil {
		return workflow.OutcomeFailed, "", fmt.Errorf("design %s: %w", t.ID, err)
	}

	var design Design
	if err := model.DecodeObject(res.Content, &design); err != nil {
		return a.skip(ctx, t, unusableReply(res, err))
	}

	files, rejected := Sanitise(design)
	if len(files) == 0 {
		return a.skip(ctx, t, skipped{
			why:    "The model proposed no usable documentation file",
			detail: "no usable files",
		})
	}

	sres, err := a.sandbox.Run(ctx, recorderFrom(ctx), forge.Spec{
		Image:       a.repo.Image,
		Command:     []string{"sh", "-c", a.CommitScript(files, t)},
		RunnerClass: a.repo.RunnerClass,
		TimeoutSecs: a.repo.TimeoutSecs,
		SecretRefs:  a.secretRefs(),
	})
	if err != nil {
		return workflow.OutcomeFailed, "", fmt.Errorf("architect: sandbox: %w", err)
	}
	out := sres.Stdout + sres.Stderr

	switch {
	case strings.Contains(out, PushedMarker):
		a.comment(ctx, t, Render(design, files, rejected))
		return workflow.OutcomeSuccess, "designed: " + strings.Join(PathsOf(files), ", "), nil

	case strings.Contains(out, NoChangeMarker):
		// Already identical on the branch. Not a failure and not worth a retry.
		a.comment(ctx, t, Marker+"\n\nThe documentation on `"+a.base()+
			"` already matches this design; nothing was committed.")
		return workflow.OutcomeSuccess, "design already current", nil

	default:
		slog.WarnContext(ctx, "architect could not commit the design",
			"ticket_id", t.ID, "output", clip(out, 400))
		return a.skip(ctx, t, skipped{
			why:    "The documentation could not be committed to `" + a.base() + "`",
			body:   clip(out, 1200),
			detail: "could not push the design",
		})
	}
}

// skipped is a design that did not happen, and why.
type skipped struct {
	why    string
	body   string
	detail string
}

// skip ends the stage without a design.
//
// EXHAUSTED, NOT FAILED: the request is still worth breaking down, and this
// stage is explicitly not a gate. Its column sends an exhausted request FORWARD
// — but it says so on the ticket rather than letting a quiet stage look like a
// working one.
func (a *Agent) skip(ctx context.Context, t ticket.Ticket, s skipped) (workflow.Outcome, string, error) {
	body := fmt.Sprintf("**Design skipped.** %s, so this request goes on to scoping undocumented.", s.why)
	if s.body != "" {
		body += "\n\n```\n" + s.body + "\n```"
	}
	a.comment(ctx, t, body)
	return workflow.OutcomeBlocked, s.detail, nil
}

// unusableReply describes a reply that could not be decoded.
//
// THE REPLY ITSELF GOES ON THE TICKET. "The model did not return usable JSON" is
// not a diagnosis — an empty reply, a refusal, a fenced block and a truncated
// object all produce it, and they need opposite fixes.
//
// TRUNCATION AND MALFORMEDNESS READ THE SAME AND ARE NOT THE SAME. A reply that
// stopped at the ceiling is a LENGTH problem; one that stopped early is the
// model's. Saying which is the difference between a comment that can be acted on
// and one that only records that something went wrong.
func unusableReply(res model.ChatResult, err error) skipped {
	why := fmt.Sprintf("the reply is not valid JSON (%v)", err)
	detail := "unparseable model output"
	if res.Truncated(MaxReplyTokens) {
		why = fmt.Sprintf("the reply was CUT OFF at the %d-token ceiling, so the JSON never closed (%v)",
			MaxReplyTokens, err)
		detail = "design reply truncated"
	}
	return skipped{
		why:    why,
		body:   fmt.Sprintf("It replied with %d completion tokens; the first of them were:\n\n%s", res.CompletionTokens, clip(strings.TrimSpace(res.Content), 800)),
		detail: detail,
	}
}

// Sanitise enforces the limits on what may be committed, and reports what it
// dropped so the ticket can say so rather than silently shipping less.
//
// THE PATH RULES ARE THE LOAD-BEARING PART: this is model output being written
// into a repository, so a path that escapes the tree, hides in a dotfile, or is
// not documentation at all is REFUSED rather than cleaned up into something
// plausible.
func Sanitise(d Design) (files []File, rejected []string) {
	seen := map[string]bool{}

	for _, f := range d.Files {
		p := path.Clean(strings.TrimSpace(f.Path))
		switch {
		case p == "" || p == ".":
			continue
		case path.IsAbs(p), strings.HasPrefix(p, ".."), strings.HasPrefix(p, "."):
			rejected = append(rejected, f.Path)
			continue
		case IsDeclarationFile(p):
			// THE ONE KIND OF CODE FILE THIS STAGE MAY WRITE, and only when it
			// declares rather than implements. See OnlyDeclarations: the point is
			// that the author's tests type-check, not that behaviour exists yet.
			if err := OnlyDeclarations(f.Content); err != nil {
				rejected = append(rejected, f.Path+" ("+err.Error()+")")
				continue
			}
			// STAMPED HERE rather than asked for in the prompt, because a marker the
			// model has to remember is one some reply will leave out.
			if !strings.Contains(f.Content, DeclarationsMarker) {
				f.Content = DeclarationsMarker + "\n" + f.Content
			}
		case !strings.HasSuffix(strings.ToLower(p), ".md"):
			rejected = append(rejected, f.Path)
			continue
		case strings.TrimSpace(f.Content) == "":
			rejected = append(rejected, f.Path)
			continue
		case len(f.Content) > MaxFileBytes:
			rejected = append(rejected, f.Path+" (too large)")
			continue
		case seen[p]:
			continue
		}
		seen[p] = true
		files = append(files, File{Path: p, Content: f.Content})
		if len(files) == MaxFiles {
			break
		}
	}
	return files, rejected
}

// PathsOf names the documents a design produced.
func PathsOf(files []File) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

// Render writes the design onto the ticket.
func Render(d Design, files []File, rejected []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n%s\n\n", Marker, strings.TrimSpace(d.Overview))
	fmt.Fprintf(&b, "Committed to the base branch: %s\n", strings.Join(PathsOf(files), ", "))
	if len(rejected) > 0 {
		// SAID RATHER THAN SWALLOWED: a document silently dropped is a gap nobody
		// knows about until an agent needs it.
		fmt.Fprintf(&b, "\nNot written (documentation at the repository root, declarations in types/): %s\n",
			strings.Join(rejected, ", "))
	}
	return b.String()
}

// ContextScript prints the tree, then every markdown file, then a sample of the
// source.
//
// ORDERING IS THE BUDGET: the clip cuts from the END, so what matters most is
// printed first. EXISTING DOCUMENTATION COMES BEFORE CODE, because the most
// damaging thing this stage can do is replace a good README with a narrower one.
func (a *Agent) ContextScript() string {
	var b strings.Builder
	b.WriteString(forge.Preamble)
	fmt.Fprintf(&b, "URL=\"${GIT_CLONE_URL:-%s}\"\n", a.repo.URL)
	b.WriteString("git clone --filter=blob:none \"$URL\" repo >/dev/null 2>&1\n")
	b.WriteString("cd repo\n")
	fmt.Fprintf(&b, "git checkout -q %s\n", forge.Quote(a.base()))

	b.WriteString("echo '=== FILES ==='\n")
	b.WriteString("git ls-files | head -200\n")

	// The guards throughout: the preamble sets -e, and a search exits non-zero
	// when a repository has no markdown or no source yet — which is exactly the
	// empty baseline this stage runs against on a new project.
	b.WriteString("echo '=== DOCUMENTATION ==='\n")
	fmt.Fprintf(&b, "for f in $(git ls-files '*.md' | head -6); do\n"+
		"  echo \"--- $f ---\"\n"+
		"  head -c %d \"$f\" || true\n"+
		"  echo\n"+
		"done\n", MaxContextFileBytes)

	b.WriteString("echo '=== SOURCE ==='\n")
	fmt.Fprintf(&b, "for f in $(git ls-files | grep -E '\\.(go|py|ts|tsx|js|rs|java|rb|c|h|cpp)$' | head -%d || true); do\n"+
		"  echo \"--- $f ---\"\n"+
		"  head -c %d \"$f\" || true\n"+
		"  echo\n"+
		"done\n", MaxRepoContextFiles, MaxContextFileBytes)
	return b.String()
}

// readRepo returns a bounded picture of the repository as it exists now.
//
// BEST-EFFORT BY DESIGN: any failure returns nothing, the design is written from
// the ticket alone, and the stage carries on.
func (a *Agent) readRepo(ctx context.Context) string {
	res, err := a.sandbox.Run(ctx, recorderFrom(ctx), forge.Spec{
		Image:       a.repo.Image,
		Command:     []string{"sh", "-c", a.ContextScript()},
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
		"and describe what is really here plus what the request adds:\n\n" + clip(out, MaxRepoContextBytes)
}

// CommitScript clones the repository, writes the documentation and pushes it to
// the base branch.
//
// IT MUST CLONE FIRST. The sandbox hands the script a bare container with a
// writable working directory and nothing in it — the clone belongs to the
// script. Assuming a checkout was already there produced "not in a git
// directory" from the first configuration command, on a design the model had
// produced perfectly well.
//
// It REBASES onto the remote before pushing and NEVER FORCE-PUSHES. This stage
// writes to the branch every other agent clones and the integrator merges into,
// so a lost update here would be invisible and would land in every sandbox
// afterwards. A push that cannot be made fast-forward is reported instead.
func (a *Agent) CommitScript(files []File, t ticket.Ticket) string {
	var b strings.Builder
	base := a.base()

	b.WriteString(forge.Preamble)
	fmt.Fprintf(&b, "URL=\"${GIT_CLONE_URL:-%s}\"\n", a.repo.URL)
	b.WriteString("git clone --filter=blob:none \"$URL\" repo >/dev/null 2>&1\n")
	b.WriteString("cd repo\n")
	fmt.Fprintf(&b, "git checkout -q %s\n", forge.Quote(base))
	b.WriteString("git config user.email 'architect@blacksmith.invalid'\n")
	b.WriteString("git config user.name 'blacksmith architect'\n")

	// BASE64, because this is model-authored content and must never be read by
	// the shell.
	for _, f := range files {
		fmt.Fprintf(&b, "mkdir -p \"$(dirname %q)\"\necho %q | base64 -d > %q\n",
			f.Path, base64.StdEncoding.EncodeToString([]byte(f.Content)), f.Path)
	}

	b.WriteString("git add -A\n")
	b.WriteString("if git diff --cached --quiet; then echo " + NoChangeMarker + "; exit 0; fi\n")
	fmt.Fprintf(&b, "git commit -q -m %s\n", forge.Quote("docs: "+clip(t.Title, 60)))
	fmt.Fprintf(&b, "git fetch -q origin %s\n", forge.Quote(base))
	b.WriteString("git rebase -q FETCH_HEAD\n")
	fmt.Fprintf(&b, "git push -q origin HEAD:%s\n", forge.Quote(base))
	b.WriteString("echo " + PushedMarker + "\n")
	return b.String()
}

func (a *Agent) base() string {
	if a.repo.Branch == "" {
		return "main"
	}
	return a.repo.Branch
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
		slog.WarnContext(ctx, "could not comment the design", "ticket_id", t.ID, "error", err)
	}
}

// renderTicket is what the stage is asked about.
func renderTicket(t ticket.Ticket) string {
	var b strings.Builder
	fmt.Fprintf(&b, "REQUEST: %s\n\n", strings.TrimSpace(t.Title))
	if d := strings.TrimSpace(t.Description); d != "" {
		b.WriteString(clip(d, 4000))
	}
	return b.String()
}

func recorderFrom(ctx context.Context) *transcript.Recorder {
	// Prefer the one Start attached; fall back to a locally attached recorder so
	// a test that wires its own still works.
	if r := transcript.RecorderFrom(ctx); r != nil {
		return r
	}
	r, _ := ctx.Value(recorderKey{}).(*transcript.Recorder)
	return r
}

type recorderKey struct{}

// WithRecorder attaches a transcript to a context.
func WithRecorder(ctx context.Context, r *transcript.Recorder) context.Context {
	return context.WithValue(ctx, recorderKey{}, r)
}

func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
