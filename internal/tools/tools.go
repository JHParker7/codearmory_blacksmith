package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/edit"
	"github.com/code-armory-app/blacksmith/internal/model"
)

// The tool names. Constants because they are matched against what the model
// sends back, and a typo in a string literal on one side of that comparison is a
// tool that silently never fires.
const (
	ReadFiles   = "read_files"
	WriteFile   = "write_file"
	UndoEdit    = "undo_edit"
	ListFiles   = "list_files"
	SearchFiles = "search_files"
	RunCommand  = "run_command"
)

// Limits on what one call may carry.
//
// These are SCHEMA limits, declared in the parameters below as well as checked
// here, so that a backend enforcing maxLength refuses an over-long argument at
// the sampler and never spends the tokens. The check remains because not every
// backend enforces it.
const (
	MaxReadPaths    = 12
	MaxAnchorChars  = 400
	MaxDeclChars    = 120
	MaxPathChars    = 200
	MaxSummaryChars = 200
	MaxSearchHits   = 200
)

// ConventionalTypes is the commit type an edit must declare. The same list the
// commit-msg hook enforces, so an agent cannot author a commit its own
// repository will reject.
var ConventionalTypes = []string{"feat", "fix", "docs", "style", "refactor", "test", "chore"}

// A Set is the tools one stage may call, bound to the workspace and sandbox they
// act on.
//
// BOUND AT CONSTRUCTION rather than passed per call, because the binding is the
// security property: a stage holding a Set whose guard refuses test files has no
// way to reach a workspace whose guard does not.
type Set struct {
	Workspace *Workspace
	Sandbox   Sandbox

	// Check is the command run_command runs. A FIXED COMMAND, not an argument:
	// letting the model choose what to run makes the sandbox a general-purpose
	// shell, and the whole reason a stage has a check is that everyone agrees in
	// advance what "it works" means.
	Check string

	// Names limits the set to these tools, in this order. Empty offers all of
	// them. Filtering here rather than at the call site means the offered tools
	// and the accepted ones cannot drift apart — a tool offered but not accepted
	// is a trap.
	Names []string

	// served is what each path held the last time it was read, so a read that
	// returns the same bytes can SAY SO. Lazily built.
	served map[string]string

	// staleReads counts consecutive reads that told the agent nothing.
	staleReads int

	// lastCall is the last (tool, arguments, result) triple, and repeats counts
	// how many times in a row it has come back unchanged.
	lastCall string
	repeats  int
}

// MaxRepeatedCalls is how many identical calls in a row may pass before the
// agent is told it is repeating itself.
//
// READ_FILES WAS THE ONLY TOOL THAT SAID SO, and the loop simply moved. Measured
// on the third two-stage attempt: an architect called list_files fifteen times
// against an empty repository, one second apart, and wrote nothing — every reply
// identical, and nothing anywhere told it so. A signal on one tool is not a
// signal, it is a hole in the shape of every other tool.
const MaxRepeatedCalls = 3

// MaxStaleReads is how many times in a row a read may return what the agent
// already had before the notice stops being gentle.
//
// A REPEAT IS NOT REFUSED. The refusal aimed at the wrong thing: an edit names
// the exact text it replaces, so an agent re-checking what it is about to match
// is doing what the edit format requires. The failure worth catching is making
// no PROGRESS, and re-reading is only its symptom.
//
// What the notice buys is the agent KNOWING the turn told it nothing, which it
// cannot otherwise tell — the contents look identical either way. Measured on
// the first live run of this loop: 76 reads against 2 writes, the same two files
// over and over, and run_command never once called. Nothing in the reply
// distinguished the 70th read from the first.
const MaxStaleReads = 3

// Definitions renders the tools for the model, in the configured order.
func (s *Set) Definitions() []model.Tool {
	all := map[string]model.Tool{
		ReadFiles: {
			Name: ReadFiles,
			Description: "Read files from the repository, with line numbers. Name every file you " +
				"need in ONE call — each call costs an iteration and you have few.",
			Parameters: object(map[string]any{
				"paths": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string", "maxLength": MaxPathChars},
					"maxItems":    MaxReadPaths,
					"description": fmt.Sprintf("Repository-relative paths, up to %d.", MaxReadPaths),
				},
			}, "paths"),
		},
		WriteFile: {
			Name: WriteFile,
			Description: `Edit ONE file. Say WHERE in exactly one way: put the exact snippet you ` +
				`are replacing in "old_str" (it must appear exactly once), OR name a whole ` +
				`function or type in "decl", OR give "start_line"/"end_line" to pick one of ` +
				`several identical lines. To write a NEW file, give "path" and "replace" and ` +
				`leave the others out. The new text always goes in "replace". NEVER put the same ` +
				`text in old_str and replace — old_str is what is there now, replace is what it ` +
				`becomes. What you do not name, you do not change. Call this again for the next file.`,
			Parameters: object(map[string]any{
				"path": map[string]any{
					"type": "string", "maxLength": MaxPathChars,
					"description": "Repository-relative path to edit.",
				},
				"old_str": map[string]any{
					"type": "string", "maxLength": MaxAnchorChars,
					"description": "A SHORT unique snippet, at most a few lines, copied exactly " +
						"from the numbered contents. It marks WHERE to change and must appear once.",
				},
				"decl": map[string]any{
					"type": "string", "maxLength": MaxDeclChars,
					"description": `The declaration to replace whole: "main", "Store.Add".`,
				},
				"start_line": map[string]any{"type": "integer", "minimum": 0},
				"end_line":   map[string]any{"type": "integer", "minimum": 0},
				"replace": map[string]any{
					"type":        "string",
					"description": "The new text. This is the only place new code goes.",
				},
				"summary": map[string]any{
					"type": "string", "maxLength": MaxSummaryChars,
					"description": "One line describing the change.",
				},
				"type": map[string]any{
					"type": "string", "enum": ConventionalTypes,
					"description": "Conventional Commits type for the commit message.",
				},
			}, "path", "replace", "summary", "type"),
		},
		UndoEdit: {
			Name: UndoEdit,
			Description: "Put the file back to what it was before your last write. Use this the " +
				"moment an edit leaves a file you no longer recognise — once the text has diverged " +
				"from what you expect, neither old_str nor a line number will find what you are " +
				"looking for, and further edits make it worse. Undo, read the file, then try again.",
			Parameters: object(nil),
		},
		ListFiles: {
			Name:        ListFiles,
			Description: "List every file in the repository.",
			Parameters:  object(nil),
		},
		SearchFiles: {
			Name:        SearchFiles,
			Description: "Search the repository for a regular expression. Returns matching lines with their paths.",
			Parameters: object(map[string]any{
				"pattern": map[string]any{"type": "string", "description": "A regular expression."},
				"glob": map[string]any{
					"type":        "string",
					"description": `Optional path suffix filter, e.g. ".go". Empty searches everything.`,
				},
			}, "pattern"),
		},
		RunCommand: {
			Name: RunCommand,
			Description: "Run this stage's check in the sandbox and return its output: " +
				"exit status, stdout and stderr. Takes no arguments — the command is fixed.",
			Parameters: object(nil),
		},
	}

	names := s.Names
	if len(names) == 0 {
		names = []string{ReadFiles, WriteFile, UndoEdit, ListFiles, SearchFiles, RunCommand}
	}
	out := make([]model.Tool, 0, len(names))
	for _, n := range names {
		if t, ok := all[n]; ok {
			out = append(out, t)
		}
	}
	return out
}

// offers reports whether a name is in this set.
func (s *Set) offers(name string) bool {
	for _, t := range s.Definitions() {
		if t.Name == name {
			return true
		}
	}
	return false
}

// Invoke runs one tool call and returns the text to hand back to the model.
//
// IT RETURNS A STRING AND NOT AN ERROR, because at this boundary a refusal is
// content: the model is meant to read it, understand what it did wrong and try
// again. Returning an error would make every refusal end the run, which is the
// opposite of what a refusal is for. Genuine transport failures — a sandbox that
// cannot be reached — come back as the error return, and those do end the turn.
func (s *Set) Invoke(ctx context.Context, name, args string) (string, error) {
	result, err := s.invoke(ctx, name, args)
	if err != nil {
		return "", err
	}
	return s.noteRepetition(name, args, result), nil
}

// noteRepetition appends a warning when a call returns exactly what the last one
// did.
//
// THE CONTENTS ARE STILL SERVED — the agent may have a good reason to look
// again, and withholding the answer would break the edit format, which requires
// quoting text exactly as it stands. What is added is the one thing the bytes
// cannot say: that they are the same bytes.
func (s *Set) noteRepetition(name, args, result string) string {
	key := name + "\x00" + args + "\x00" + result
	if key != s.lastCall {
		s.lastCall, s.repeats = key, 0
		return result
	}
	s.repeats++
	if s.repeats < MaxRepeatedCalls {
		return result
	}
	// read_files already explains itself in content-aware terms; a second notice
	// saying the same thing less precisely is noise.
	if strings.Contains(result, "NOTE: you already had") {
		return result
	}

	next := "Change something before asking again."
	switch {
	case s.offers(WriteFile) && s.offers(RunCommand):
		next = "Call " + WriteFile + " to change something, or " + RunCommand +
			" to find out whether what you have already written works."
	case s.offers(WriteFile):
		next = "Call " + WriteFile + " to write what you were asked for. Nothing else will " +
			"move this on."
	}
	return result + fmt.Sprintf(
		"\n\nNOTE: that is %d times in a row that %s has returned exactly this, and it will keep "+
			"returning it. Repeating the call cannot tell you anything new. %s",
		s.repeats+1, name, next)
}

func (s *Set) invoke(ctx context.Context, name, args string) (string, error) {
	if !s.offers(name) {
		return fmt.Sprintf(
			"Error: there is no tool called %q at this stage. The tools you have are listed in "+
				"this request; call one of those.", name), nil
	}

	switch name {
	case ReadFiles:
		var a struct {
			Paths []string `json:"paths"`
		}
		if err := json.Unmarshal([]byte(args), &a); err != nil {
			return badArgs(name, err), nil
		}
		return s.readFiles(a.Paths), nil

	case WriteFile:
		var e edit.Edit
		var meta struct {
			Summary string `json:"summary"`
			Type    string `json:"type"`
		}
		if err := json.Unmarshal([]byte(args), &e); err != nil {
			return badArgs(name, err), nil
		}
		_ = json.Unmarshal([]byte(args), &meta)
		if !validType(meta.Type) {
			return fmt.Sprintf("Error: \"type\" must be one of %s.", strings.Join(ConventionalTypes, ", ")), nil
		}
		if len(meta.Summary) > MaxSummaryChars {
			return fmt.Sprintf("Error: \"summary\" is %d characters, and the limit is %d. One line.",
				len(meta.Summary), MaxSummaryChars), nil
		}
		line, err := s.Workspace.ApplyEdit(e)
		if err != nil {
			return "Error: " + err.Error(), nil
		}
		return line + " " + meta.Summary, nil

	case UndoEdit:
		p, ok := s.Workspace.Undo()
		if !ok {
			return "Error: nothing to undo — no write has been made yet.", nil
		}
		return fmt.Sprintf("Restored %s to what it was before your last write.", p), nil

	case ListFiles:
		paths := s.Workspace.Paths()
		if len(paths) == 0 {
			return "The repository is empty.", nil
		}
		return strings.Join(paths, "\n"), nil

	case SearchFiles:
		var a struct {
			Pattern string `json:"pattern"`
			Glob    string `json:"glob"`
		}
		if err := json.Unmarshal([]byte(args), &a); err != nil {
			return badArgs(name, err), nil
		}
		return s.search(a.Pattern, a.Glob), nil

	case RunCommand:
		if s.Sandbox == nil {
			return "Error: no sandbox is attached at this stage, so nothing can be run.", nil
		}
		out, err := s.Sandbox.Run(ctx, s.Workspace.Files(), s.Check)
		if err != nil {
			// A TRANSPORT FAILURE, not a refusal. Handed back as an error so the
			// caller ends the turn rather than letting the model reason about a
			// cluster outage as though it were a test failure.
			return "", fmt.Errorf("running %q: %w", s.Check, err)
		}
		return fmt.Sprintf("$ %s\nexit %d\n%s%s", s.Check, out.ExitCode, out.Stdout, out.Stderr), nil
	}
	return "", fmt.Errorf("unhandled tool %q", name)
}

func (s *Set) readFiles(paths []string) string {
	if len(paths) == 0 {
		return "Error: no paths given. Name the files you want to read."
	}
	clean, err := edit.ValidatePaths(paths, MaxReadPaths)
	if err != nil {
		return "Error: " + err.Error()
	}
	if s.served == nil {
		s.served = map[string]string{}
	}

	var b strings.Builder
	var fresh int
	for _, p := range clean {
		fmt.Fprintf(&b, "=== %s ===\n", p)
		content, ok := s.Workspace.Read(p)
		if !ok {
			// NAMING THE NEIGHBOURS, not just the absence. A model that misremembers
			// a path retries the same guess when told only "not found"; shown what is
			// actually there, it corrects on the next turn.
			b.WriteString("Error: no such file. " + s.nearby(p) + "\n\n")
			// A miss is not a stale read: it did tell the agent something.
			fresh++
			continue
		}
		if had, seen := s.served[p]; !seen || had != content {
			fresh++
		}
		s.served[p] = content
		// Recorded so the prompt carries this file's contents from now on. A read
		// whose result scrolls out of the trail must not take the file with it.
		s.Workspace.Seen(p)
		b.WriteString(numbered(content))
		b.WriteString("\n\n")
	}

	// THE CONTENTS ARE STILL SERVED. Withholding them would break the edit
	// format, which requires quoting text exactly as it stands; what is added is
	// the one thing the bytes cannot say, which is that they have not changed.
	if fresh == 0 {
		s.staleReads++
		b.WriteString(s.staleNotice(clean))
	} else {
		s.staleReads = 0
	}
	return strings.TrimRight(b.String(), "\n")
}

func (s *Set) staleNotice(paths []string) string {
	notice := fmt.Sprintf(
		"NOTE: you already had %s — the contents above are current, and this read returned the "+
			"same bytes. It told you nothing new.", strings.Join(paths, ", "))
	if s.staleReads >= MaxStaleReads {
		notice += fmt.Sprintf(" You have now re-read %d times in a row without changing anything. "+
			"Nothing new can come from asking again. Call %s to change something, or %s to find "+
			"out whether what you have already written works.",
			s.staleReads, WriteFile, RunCommand)
	}
	return notice
}

// nearby names the files in the same directory, so a wrong path is correctable.
func (s *Set) nearby(p string) string {
	dir := parentDir(p)
	var siblings []string
	for _, q := range s.Workspace.Paths() {
		if parentDir(q) == dir {
			siblings = append(siblings, q)
		}
	}
	if len(siblings) == 0 {
		return "Call list_files to see what the repository holds."
	}
	if len(siblings) > MaxReadPaths {
		siblings = siblings[:MaxReadPaths]
	}
	where := "the repository root"
	if dir != "" {
		where = dir
	}
	return fmt.Sprintf("In %s there is: %s.", where, strings.Join(siblings, ", "))
}

func (s *Set) search(pattern, glob string) string {
	if strings.TrimSpace(pattern) == "" {
		return "Error: no pattern given."
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Sprintf("Error: %q is not a valid regular expression: %v", pattern, err)
	}
	var hits []string
	for _, p := range s.Workspace.Paths() {
		if glob != "" && !strings.HasSuffix(p, glob) {
			continue
		}
		content, _ := s.Workspace.Read(p)
		for i, line := range strings.Split(content, "\n") {
			if re.MatchString(line) {
				hits = append(hits, fmt.Sprintf("%s:%d: %s", p, i+1, line))
			}
		}
	}
	if len(hits) == 0 {
		return "No matches found."
	}
	if len(hits) > MaxSearchHits {
		extra := len(hits) - MaxSearchHits
		hits = hits[:MaxSearchHits]
		return strings.Join(hits, "\n") +
			fmt.Sprintf("\n… %d more matches. Narrow the pattern.", extra)
	}
	return strings.Join(hits, "\n")
}

// numbered prefixes each line with its number.
//
// Because every edit refusal tells the model to copy from "the numbered
// contents" and to disambiguate repeated lines by line number. Showing the file
// without them makes that advice unfollowable.
func numbered(text string) string {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	width := len(fmt.Sprint(len(lines)))
	var b strings.Builder
	for i, l := range lines {
		fmt.Fprintf(&b, "%*d\t%s\n", width, i+1, l)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func validType(t string) bool {
	for _, c := range ConventionalTypes {
		if t == c {
			return true
		}
	}
	return false
}

func badArgs(name string, err error) string {
	return fmt.Sprintf(
		"Error: the arguments to %s were not valid JSON (%v). Send the arguments as a JSON object "+
			"matching this tool's parameters.", name, err)
}

// object builds a JSON Schema object with additionalProperties closed.
//
// CLOSED ON PURPOSE. An open object lets a model invent a field, have it
// accepted, and never learn that the field did nothing — which reads to it as
// the edit having been applied.
func object(props map[string]any, required ...string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	if required == nil {
		required = []string{}
	}
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
}
