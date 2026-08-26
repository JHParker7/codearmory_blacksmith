package architect

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/forge"
	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/ticket"
	"github.com/code-armory-app/blacksmith/internal/workflow"
)

// sandbox answers each run in turn: the repository read, then the commit.
type sandbox struct {
	results []forge.Result
	errs    []error
	specs   []forge.Spec
}

func (s *sandbox) Run(_ context.Context, _ forge.Recorder, spec forge.Spec) (forge.Result, error) {
	s.specs = append(s.specs, spec)
	i := len(s.specs) - 1
	if i < len(s.errs) && s.errs[i] != nil {
		return forge.Result{}, s.errs[i]
	}
	if i < len(s.results) {
		return s.results[i], nil
	}
	return forge.Result{Status: forge.StatusCompleted}, nil
}

type gateway struct {
	res   model.ChatResult
	err   error
	req   model.ChatRequest
	calls int
}

func (g *gateway) Chat(_ context.Context, _ model.Class, req model.ChatRequest) (model.ChatResult, error) {
	g.calls++
	g.req = req
	if g.err != nil {
		return model.ChatResult{}, g.err
	}
	return g.res, nil
}

type board struct{ comments []string }

func (b *board) AddComment(_ context.Context, _, body string) (ticket.Comment, error) {
	b.comments = append(b.comments, body)
	return ticket.Comment{Body: body}, nil
}

func (b *board) saidAny(want string) bool {
	for _, c := range b.comments {
		if strings.Contains(c, want) {
			return true
		}
	}
	return false
}

func repo() config.Repo {
	return config.Repo{
		URL: "https://git.example/org/repo", Branch: "main",
		Image: "golang:1.25", RunnerClass: "agent-dev",
		SecretRef: "git:https://git.example/org/repo",
	}
}

func request() ticket.Ticket {
	return ticket.Ticket{
		ID: "t-1", Title: "Build a task manager", Status: workflow.ColInbox,
		Description: "It should store tasks and serve them over HTTP.",
	}
}

func design(files ...File) model.ChatResult {
	b, _ := json.Marshal(Design{Overview: "a task manager", Files: files})
	return model.ChatResult{Content: string(b), CompletionTokens: 900}
}

func out(s string) forge.Result {
	return forge.Result{Status: forge.StatusCompleted, ExitCode: 0, Stdout: s}
}

// THE DESIGN IS COMMITTED TO THE BASE BRANCH, so it is simply present in the
// tree every later sandbox clones.
func TestADesignIsWrittenAndRecorded(t *testing.T) {
	sb := &sandbox{results: []forge.Result{out(""), out(PushedMarker)}}
	g := &gateway{res: design(
		File{Path: "README.md", Content: "# Task manager\n"},
		File{Path: "ARCHITECTURE.md", Content: "## Pieces\n\nA Store and a Server.\n"},
	)}
	b := &board{}

	status, detail, err := New(g, sb, b, model.ClassSmall, repo()).Handle(context.Background(), request())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if status != workflow.OutcomeSuccess {
		t.Errorf("status = %q", status)
	}
	for _, want := range []string{"README.md", "ARCHITECTURE.md"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail = %q, want it to name %s", detail, want)
		}
	}
	if !b.saidAny(Marker) {
		t.Error("the ticket does not carry the design marker; it could be designed twice")
	}
	if !b.saidAny("a task manager") {
		t.Error("the ticket does not carry the overview")
	}
}

// THE REPOSITORY IS READ FIRST. The model writes whole files, so an architect
// that had never seen the existing README would overwrite it with a description
// of one feature.
func TestTheExistingDocumentationIsShownBeforeTheDesign(t *testing.T) {
	existing := "=== FILES ===\nREADME.md\n=== DOCUMENTATION ===\n--- README.md ---\n# The existing project\n"
	sb := &sandbox{results: []forge.Result{out(existing), out(PushedMarker)}}
	g := &gateway{res: design(File{Path: "README.md", Content: "# Updated\n"})}

	New(g, sb, &board{}, model.ClassSmall, repo()).Handle(context.Background(), request())

	if len(sb.specs) != 2 {
		t.Fatalf("%d sandboxes ran, want the read and the commit", len(sb.specs))
	}
	var sawExisting bool
	for _, m := range g.req.Messages {
		if strings.Contains(m.Content, "# The existing project") {
			sawExisting = true
		}
	}
	if !sawExisting {
		t.Error("the model was not shown the documentation it is about to replace")
	}
	// And it must be told to update rather than replace.
	joined := ""
	for _, m := range g.req.Messages {
		joined += m.Content
	}
	if !strings.Contains(joined, "Update this documentation rather than replacing it") {
		t.Error("the model was not told to update rather than replace")
	}
}

// READING THE REPOSITORY IS BEST-EFFORT: a design written from the ticket alone
// is worth more than no design.
func TestADesignIsStillWrittenWhenTheRepositoryCannotBeRead(t *testing.T) {
	sb := &sandbox{
		results: []forge.Result{{}, out(PushedMarker)},
		errs:    []error{errors.New("the sandbox never booted"), nil},
	}
	g := &gateway{res: design(File{Path: "README.md", Content: "# From the ticket alone\n"})}

	status, _, err := New(g, sb, &board{}, model.ClassSmall, repo()).Handle(context.Background(), request())
	if err != nil {
		t.Fatalf("a failed repository read stopped the stage: %v", err)
	}
	if status != workflow.OutcomeSuccess {
		t.Errorf("status = %q", status)
	}
	if g.calls != 1 {
		t.Error("the model was not asked after the repository read failed")
	}
}

// THIS STAGE IS NOT A GATE. A request nobody documented is worth more than a
// request nobody builds, so a skipped design still says so on the ticket and
// sends the request onward.
func TestEveryWayADesignCanFailStillLetsTheRequestThrough(t *testing.T) {
	cases := map[string]struct {
		res     model.ChatResult
		commit  string
		wantSay string
	}{
		"an unparseable reply": {
			res:     model.ChatResult{Content: "I could not design this.", CompletionTokens: 20},
			wantSay: "not valid JSON",
		},
		"no usable files": {
			// A YAML FILE, NOT A .go ONE. Since the architect began declaring types
			// a Go file is a legitimate output, so "package main" is no longer an
			// example of producing nothing usable.
			res:     design(File{Path: ".github/workflows/ci.yml", Content: "evil\n"}),
			wantSay: "no usable documentation file",
		},
		"the push failed": {
			res:     design(File{Path: "README.md", Content: "# x\n"}),
			commit:  "error: failed to push some refs",
			wantSay: "could not be committed",
		},
	}

	for name, c := range cases {
		sb := &sandbox{results: []forge.Result{out(""), out(c.commit)}}
		g := &gateway{res: c.res}
		b := &board{}

		status, _, err := New(g, sb, b, model.ClassSmall, repo()).Handle(context.Background(), request())
		if err != nil {
			t.Errorf("%s: Handle: %v", name, err)
			continue
		}
		// Blocked routes to this stage's Exhausted column, which sends the request
		// FORWARD to scoping.
		if status != workflow.OutcomeBlocked {
			t.Errorf("%s: status = %q", name, status)
		}
		if !b.saidAny("Design skipped") {
			t.Errorf("%s: the ticket does not say the design was skipped", name)
		}
		if !b.saidAny("goes on to scoping undocumented") {
			t.Errorf("%s: the ticket does not say what happens next", name)
		}
		if !b.saidAny(c.wantSay) {
			t.Errorf("%s: the ticket does not say %q", name, c.wantSay)
		}
	}
}

// TRUNCATION AND MALFORMEDNESS READ THE SAME AND ARE NOT THE SAME. One is a
// length problem and one is the model's, and they need opposite fixes.
func TestATruncatedReplyIsDistinguishedFromAMalformedOne(t *testing.T) {
	t.Run("truncated", func(t *testing.T) {
		sb := &sandbox{results: []forge.Result{out("")}}
		g := &gateway{res: model.ChatResult{
			Content:          `{"overview":"a task manager","files":[{"path":"README.md","content":"# Task`,
			CompletionTokens: MaxReplyTokens,
		}}
		b := &board{}

		_, detail, _ := New(g, sb, b, model.ClassSmall, repo()).Handle(context.Background(), request())
		if !strings.Contains(detail, "truncated") {
			t.Errorf("detail = %q, want it to name the truncation", detail)
		}
		if !b.saidAny("CUT OFF") {
			t.Error("the ticket does not say the reply hit the ceiling")
		}
	})

	t.Run("malformed", func(t *testing.T) {
		sb := &sandbox{results: []forge.Result{out("")}}
		g := &gateway{res: model.ChatResult{Content: "I refuse.", CompletionTokens: 5}}
		b := &board{}

		_, detail, _ := New(g, sb, b, model.ClassSmall, repo()).Handle(context.Background(), request())
		if strings.Contains(detail, "truncated") {
			t.Errorf("detail = %q, want it not to blame the ceiling", detail)
		}
		// THE REPLY ITSELF GOES ON THE TICKET: without the text there is nothing to
		// reason from but the timing.
		if !b.saidAny("I refuse.") {
			t.Error("the ticket does not carry what the model actually said")
		}
	})
}

// THE PATH RULES ARE LOAD-BEARING: this is model output being written into a
// repository every later agent clones.
func TestOnlyMarkdownAtTheRootIsEverCommitted(t *testing.T) {
	files, rejected := Sanitise(Design{Files: []File{
		{Path: "README.md", Content: "# ok\n"},
		{Path: "docs/GUIDE.md", Content: "# also ok\n"},
		{Path: "/etc/cron.d/x", Content: "evil\n"},
		{Path: "../../outside.md", Content: "evil\n"},
		{Path: ".github/workflows/ci.yml", Content: "evil\n"},
		{Path: "types.go", Content: "package main\n\ntype Ticket struct{ ID string }\n\nfunc NewStore() *Store { panic(\"not implemented\") }\n"},
		{Path: "worker.go", Content: "package main\n\nfunc Work() int { return 41 + 1 }\n"},
		{Path: "store_test.go", Content: "package main\n"},
		{Path: "empty.md", Content: "   \n"},
	}})

	got := map[string]bool{}
	for _, f := range files {
		got[f.Path] = true
	}
	if !got["README.md"] || !got["docs/GUIDE.md"] {
		t.Errorf("legitimate documentation was dropped: %v", PathsOf(files))
	}
	// DECLARATIONS ARE NOW A LEGITIMATE OUTPUT, so the filter is no longer "is it
	// markdown" — it is "is it documentation, or a declaration with no behaviour".
	if !got["types.go"] {
		t.Errorf("a declarations file was dropped: %v", PathsOf(files))
	}
	for _, bad := range []string{
		"/etc/cron.d/x", "../../outside.md", ".github/workflows/ci.yml",
		"worker.go",     // has a body: that is the developer's work
		"store_test.go", // is a test: that is the author's work
	} {
		if got[bad] {
			t.Errorf("%q was accepted for commit", bad)
		}
	}
	// SAID RATHER THAN SWALLOWED, so the ticket can report what was dropped.
	if len(rejected) < 4 {
		t.Errorf("rejected = %v, want every refusal named", rejected)
	}
}

func TestADesignIsBoundedInCountAndSize(t *testing.T) {
	var many []File
	for i := 0; i < 20; i++ {
		many = append(many, File{Path: string(rune('a'+i)) + ".md", Content: "# x\n"})
	}
	files, _ := Sanitise(Design{Files: many})
	if len(files) != MaxFiles {
		t.Errorf("kept %d files, want at most %d", len(files), MaxFiles)
	}

	huge := strings.Repeat("x", MaxFileBytes+1)
	kept, rejected := Sanitise(Design{Files: []File{{Path: "BIG.md", Content: huge}}})
	if len(kept) != 0 {
		t.Error("an oversized document was committed")
	}
	if len(rejected) != 1 || !strings.Contains(rejected[0], "too large") {
		t.Errorf("rejected = %v, want it to say why", rejected)
	}
}

func TestADuplicatePathIsWrittenOnce(t *testing.T) {
	files, _ := Sanitise(Design{Files: []File{
		{Path: "README.md", Content: "# first\n"},
		{Path: "./README.md", Content: "# second\n"},
	}})
	if len(files) != 1 {
		t.Fatalf("kept %d files for one path", len(files))
	}
	if files[0].Content != "# first\n" {
		t.Errorf("kept %q, want the first", files[0].Content)
	}
}

// ALREADY IDENTICAL IS NOT A FAILURE and not worth a retry.
func TestADesignThatChangesNothingIsASuccess(t *testing.T) {
	sb := &sandbox{results: []forge.Result{out(""), out(NoChangeMarker)}}
	g := &gateway{res: design(File{Path: "README.md", Content: "# unchanged\n"})}
	b := &board{}

	status, detail, err := New(g, sb, b, model.ClassSmall, repo()).Handle(context.Background(), request())
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if status != workflow.OutcomeSuccess {
		t.Errorf("status = %q", status)
	}
	if !strings.Contains(detail, "already current") {
		t.Errorf("detail = %q", detail)
	}
	if b.saidAny("Design skipped") {
		t.Error("an unchanged design was reported as skipped")
	}
}

// A REQUEST IS DESIGNED ONCE. A re-poll of the same column must not design it
// twice.
func TestARequestIsDesignedOnlyOnce(t *testing.T) {
	a := New(&gateway{}, &sandbox{}, &board{}, model.ClassSmall, repo())

	fresh := request()
	if !a.Wants(fresh) {
		t.Error("the stage refused an undesigned request")
	}
	designed := request()
	designed.Comments = []ticket.Comment{{Body: Marker + "\n\nan overview"}}
	if a.Wants(designed) {
		t.Error("the stage wanted a request it had already designed")
	}
	if !Designed(designed) {
		t.Error("Designed() did not recognise its own marker")
	}
}

// PROSE IS THE ONE OUTPUT HERE WITH NO SINGLE CORRECT WORDING, and greedy
// decoding over two pages of it falls into repetition.
func TestTheDesignIsSampledWithSomeNoiseButAFixedShape(t *testing.T) {
	sb := &sandbox{results: []forge.Result{out(""), out(PushedMarker)}}
	g := &gateway{res: design(File{Path: "README.md", Content: "# x\n"})}

	New(g, sb, &board{}, model.ClassSmall, repo()).Handle(context.Background(), request())

	if g.req.Temperature <= 0 {
		t.Error("the design is decoded greedily; two pages of prose fall into repetition")
	}
	// The SHAPE is still not left to chance.
	if g.req.Schema == nil {
		t.Error("the reply shape was left unconstrained")
	}
	if g.req.MaxTokens < 4000 {
		t.Errorf("max tokens = %d; two documents plus their escaping do not fit", g.req.MaxTokens)
	}
}

func TestTheScriptsAreValidShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	a := New(&gateway{}, &sandbox{}, &board{}, model.ClassSmall, repo())

	scripts := map[string]string{
		"context": a.ContextScript(),
		"commit": a.CommitScript([]File{
			{Path: "README.md", Content: "# Title\n\n```bash\ngo build ./...\n```\n"},
		}, request()),
		// A title carrying an apostrophe: an unbalanced quote makes the shell exit
		// on a syntax error with nothing run.
		"commit with prose in the title": a.CommitScript(
			[]File{{Path: "README.md", Content: "# x\n"}},
			ticket.Ticket{ID: "t-1", Title: "this project's own docs"}),
	}
	for name, s := range scripts {
		if o, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("%s script is not valid shell: %v\n%s", name, err, o)
		}
	}
}

// IT MUST CLONE FIRST: the sandbox hands the script a bare container, and
// assuming a checkout produced "not in a git directory" on a design the model
// had produced perfectly well.
func TestTheCommitScriptClonesBeforeItConfigures(t *testing.T) {
	s := New(&gateway{}, &sandbox{}, &board{}, model.ClassSmall, repo()).
		CommitScript([]File{{Path: "README.md", Content: "# x\n"}}, request())

	clone := strings.Index(s, "git clone")
	conf := strings.Index(s, "git config")
	if clone < 0 || conf < 0 {
		t.Fatal("the script does not both clone and configure")
	}
	if clone > conf {
		t.Error("the script configures git before there is a repository")
	}
}

// IT NEVER FORCE-PUSHES to the branch every other agent clones: a lost update
// here would be invisible and would land in every sandbox afterwards.
func TestTheCommitScriptRebasesAndNeverForces(t *testing.T) {
	s := New(&gateway{}, &sandbox{}, &board{}, model.ClassSmall, repo()).
		CommitScript([]File{{Path: "README.md", Content: "# x\n"}}, request())

	if strings.Contains(s, "--force") || strings.Contains(s, " -f ") {
		t.Error("the script force-pushes to the base branch")
	}
	rebase := strings.Index(s, "git rebase")
	push := strings.Index(s, "git push")
	if rebase < 0 || push < 0 || rebase > push {
		t.Error("the script does not rebase onto the remote before pushing")
	}
	// It must say nothing was committed rather than pushing an empty change.
	if !strings.Contains(s, NoChangeMarker) {
		t.Error("the script cannot report that nothing changed")
	}
}

// MODEL-AUTHORED CONTENT IS NEVER READ BY THE SHELL.
func TestTheDocumentsAreWrittenThroughBase64(t *testing.T) {
	body := "# Title\n\n```bash\n$(rm -rf /) && echo `whoami`\n```\n"
	s := New(&gateway{}, &sandbox{}, &board{}, model.ClassSmall, repo()).
		CommitScript([]File{{Path: "README.md", Content: body}}, request())

	if strings.Contains(s, "rm -rf /") {
		t.Error("model-authored content reached the script as text")
	}
	if !strings.Contains(s, base64.StdEncoding.EncodeToString([]byte(body))) {
		t.Error("the document was not written through base64")
	}
}

// A README WITH A FENCED SNIPPET IN IT IS THE NORM, and taking the first fence
// would throw the object away and return the shell snippet.
func TestADesignCarryingAFencedBlockIsNotMangled(t *testing.T) {
	body := "# Title\n\n```bash\ngo build ./...\n```\n"
	raw, _ := json.Marshal(Design{Overview: "x", Files: []File{{Path: "README.md", Content: body}}})

	var got Design
	if err := model.DecodeObject(string(raw), &got); err != nil {
		t.Fatalf("a design containing a fence was rejected: %v", err)
	}
	if !strings.Contains(got.Files[0].Content, "go build ./...") {
		t.Errorf("the fenced block was lost: %q", got.Files[0].Content)
	}
}

// THE EMPTY BASELINE IS WHAT THIS STAGE RUNS AGAINST on a new project, and a
// search that finds nothing exits non-zero under set -e.
func TestTheContextScriptSurvivesAnEmptyRepository(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	s := New(&gateway{}, &sandbox{}, &board{}, model.ClassSmall, repo()).ContextScript()

	// Take the reading half — everything after the checkout — and run it under
	// set -e in an empty directory with a stub `git` that lists nothing.
	_, after, ok := strings.Cut(s, "echo '=== FILES ==='")
	if !ok {
		t.Fatal("the context script has no file listing")
	}
	dir := t.TempDir()
	if err := exec.Command("sh", "-c",
		"printf '#!/bin/sh\\nexit 0\\n' > "+dir+"/git && chmod +x "+dir+"/git").Run(); err != nil {
		t.Fatalf("preparing the stub: %v", err)
	}

	cmd := exec.Command("sh", "-c", "set -e\necho '=== FILES ==='"+after)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "PATH="+dir+":"+strings.TrimSpace(mustPath(t)))
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("the context script failed against an empty repository: %v\n%s", err, o)
	}
}

func mustPath(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("sh", "-c", "echo $PATH").Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestTheStageIdentifiesItself(t *testing.T) {
	a := New(&gateway{}, &sandbox{}, &board{}, model.ClassSmall, config.Repo{URL: "x"})
	if a.Role() != workflow.RoleArchitect {
		t.Errorf("Role() = %q", a.Role())
	}
	if a.Class() != model.ClassSmall {
		t.Errorf("Class() = %q", a.Class())
	}
	// A host with no repository cannot design.
	if _, _, err := New(&gateway{}, &sandbox{}, &board{}, model.ClassSmall, config.Repo{}).
		Handle(context.Background(), request()); err == nil {
		t.Error("a host with no repository configured designed something")
	}
}

func TestThePromptAsksForAPlanRatherThanADescription(t *testing.T) {
	for _, want := range []string{
		"WILL be built",
		"Do not describe the current empty repository",
		"anything you leave out is deleted",
		// THE STAGE NO LONGER WRITES DOCUMENTATION ONLY. It declares the shared
		// types as well, so the tests a later stage writes type-check — see
		// OnlyDeclarations. What has to stay said is the LIMIT on that: shapes,
		// not behaviour.
		"DECLARES and does not implement",
		"empty or a single panic",
		"may be a _test.go file",
	} {
		if !strings.Contains(SystemPrompt, want) {
			t.Errorf("the prompt does not say %q", want)
		}
	}
}
