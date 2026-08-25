package dev

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
)

// branches returns the oneOf alternatives of a schema node.
func branches(t *testing.T, node map[string]any) []map[string]any {
	t.Helper()
	raw, ok := node["oneOf"].([]any)
	if !ok {
		t.Fatalf("node has no oneOf: %v", node)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, b := range raw {
		m, ok := b.(map[string]any)
		if !ok {
			t.Fatalf("a oneOf branch is not an object: %v", b)
		}
		out = append(out, m)
	}
	return out
}

func props(t *testing.T, node map[string]any) map[string]any {
	t.Helper()
	p, ok := node["properties"].(map[string]any)
	if !ok {
		t.Fatalf("node has no properties: %v", node)
	}
	return p
}

func required(t *testing.T, node map[string]any) []string {
	t.Helper()
	r, ok := node["required"].([]string)
	if !ok {
		t.Fatalf("node has no required list: %v", node)
	}
	return r
}

// branchFor finds the alternative whose "action" enum names this action.
func branchFor(t *testing.T, action string) map[string]any {
	t.Helper()
	for _, b := range branches(t, Schema().Schema) {
		p := props(t, b)
		enum, _ := p["action"].(map[string]any)["enum"].([]string)
		if len(enum) == 1 && enum[0] == action {
			return b
		}
	}
	t.Fatalf("the schema has no branch for %q", action)
	return nil
}

// ONE SHAPE PER ACTION, because a single object with everything optional is not
// a constraint at all.
//
// This was one flat object requiring only "action". Harmless as a fallback and
// the whole contract once the turn stopped offering tools: the grammar admitted
// {"action":"write_files"} — 25 refusals of "write_files with no edits" in the
// first window after the switch, every one schema-valid.
func TestAnEditlessWriteIsNotExpressible(t *testing.T) {
	write := branchFor(t, ActionWriteFile)
	for _, want := range []string{"action", "path", "replace", "summary", "type"} {
		if !slices.Contains(required(t, write), want) {
			t.Errorf("write_file does not require %q; an empty write is schema-valid", want)
		}
	}
	// An action's own branch may not carry another action's fields, or the
	// branching buys nothing.
	read := branchFor(t, ActionReadFiles)
	if _, has := props(t, read)["edits"]; has {
		t.Error("read_files can carry edits")
	}
	if !slices.Contains(required(t, read), "paths") {
		t.Error("read_files does not require paths; a read of nothing is schema-valid")
	}
	if props(t, read)["paths"].(map[string]any)["minItems"] != 1 {
		t.Error("read_files admits an empty path list")
	}

	// EVERY BRANCH IS CLOSED. An open object would let the model add a field the
	// harness then ignores, which reads to it as the field having been accepted.
	for _, b := range branches(t, Schema().Schema) {
		if b["additionalProperties"] != false {
			t.Errorf("a branch admits properties nobody defined: %v", b["properties"])
		}
	}
}

// EVERY FIELD IS BOUNDED, because this becomes a GRAMMAR and an unbounded string
// in a grammar is an unbounded reply. Measured when a backend without tool
// support first fell through here: the model filled "type" with
// "replace_all_content_in_file_if_exists_…" and kept going for 5,000 tokens and
// 348 seconds — schema-valid the whole way.
//
// "replace" IS THE ONE DELIBERATE EXCEPTION: it is where new code goes, and a
// cap on it would cap the size of a change rather than the size of a mistake.
func TestEveryStringInTheGrammarIsBoundedExceptTheCodeItself(t *testing.T) {
	var unbounded []string

	var walk func(node any, path string)
	walk = func(node any, path string) {
		m, ok := node.(map[string]any)
		if !ok {
			if list, ok := node.([]any); ok {
				for i, item := range list {
					walk(item, fmt.Sprintf("%s[%d]", path, i))
				}
			}
			return
		}
		if m["type"] == "string" {
			_, capped := m["maxLength"]
			_, enumerated := m["enum"]
			if !capped && !enumerated && !strings.HasSuffix(path, "replace") {
				unbounded = append(unbounded, path)
			}
		}
		for _, key := range []string{"properties", "items"} {
			if child, has := m[key]; has {
				if sub, ok := child.(map[string]any); ok && key == "properties" {
					for name, v := range sub {
						walk(v, path+"."+name)
					}
					continue
				}
				walk(child, path)
			}
		}
		if list, has := m["oneOf"].([]any); has {
			for i, b := range list {
				walk(b, fmt.Sprintf("%s|%d", path, i))
			}
		}
	}
	walk(Schema().Schema, "")

	sort.Strings(unbounded)
	if len(unbounded) > 0 {
		t.Errorf("unbounded strings in the grammar: %v", unbounded)
	}
}

// ONE SHAPE PER WAY OF POINTING AT THE CODE, because putting every address in
// one object is what made the copy inevitable: with old_str and replace both
// required and adjacent, a constrained sampler's highest-probability
// continuation after a long quote is that same quote again. Measured: old_str
// identical to replace in 39 of 47 turns, and the no-progress ceiling then
// killed the ticket — twice, taking the board with it.
func TestNoEditShapeLetsAQuoteSitBesideItsOwnReplacement(t *testing.T) {
	items, ok := EditsSchema()["items"].(map[string]any)
	if !ok {
		t.Fatal("the edits schema has no item shape")
	}
	shapes := branches(t, items)
	if len(shapes) != 3 {
		t.Fatalf("%d edit shapes, want decl, anchor and lines", len(shapes))
	}

	var sawDecl, sawAnchor, sawLines bool
	for _, s := range shapes {
		p := props(t, s)
		_, hasOld := p["old_str"]
		_, hasDecl := p["decl"]
		_, hasStart := p["start_line"]

		switch {
		case hasDecl:
			sawDecl = true
			// THE DECL SHAPE HAS NO old_str TO COPY.
			if hasOld {
				t.Error("the declaration shape carries a quote")
			}
		case hasOld:
			sawAnchor = true
			// THE ANCHOR IS SHORT, so it cannot be a duplicate of a long
			// replacement. Without the cap, quoting a whole function is expressible
			// again and the copy comes back.
			cap, capped := p["old_str"].(map[string]any)["maxLength"]
			if !capped {
				t.Fatal("the anchor is unbounded; a whole function can be quoted")
			}
			if cap != MaxAnchorChars {
				t.Errorf("the anchor cap is %v, want %d", cap, MaxAnchorChars)
			}
			if hasDecl || hasStart {
				t.Error("the anchor shape also carries another address")
			}
		case hasStart:
			sawLines = true
			// THE LINE SHAPE CARRIES NO QUOTE AT ALL.
			if hasOld || hasDecl {
				t.Error("the line shape also carries another address")
			}
		}

		// Every shape must name a file and say what goes there, or it addresses
		// nothing.
		for _, want := range []string{"path", "replace"} {
			if !slices.Contains(required(t, s), want) {
				t.Errorf("an edit shape does not require %q", want)
			}
		}
		if s["additionalProperties"] != false {
			t.Error("an edit shape admits fields nobody defined")
		}
	}
	if !sawDecl || !sawAnchor || !sawLines {
		t.Errorf("shapes present: decl=%v anchor=%v lines=%v", sawDecl, sawAnchor, sawLines)
	}
}

// THE SAME SHAPE REACHES THE MODEL BOTH WAYS. Two channels describing different
// shapes is how the last format ended up with a grammar and a tool definition
// that disagreed.
//
// Compared field by field rather than as one object: the tool carries its own
// descriptions and the grammar carries its own bounds, so the shapes are equal
// in the thing that matters — which fields exist and what type each is — and
// deliberately not identical in prose.
func TestTheToolAndTheGrammarDescribeOneEditShape(t *testing.T) {
	var tool map[string]any
	for _, tl := range Tools(ModeDevelop) {
		if tl.Name == ActionWriteFile {
			tool, _ = tl.Parameters["properties"].(map[string]any)
		}
	}
	if tool == nil {
		t.Fatal("the write tool defines no properties")
	}
	grammar := props(t, branchFor(t, ActionWriteFile))

	for _, field := range []string{"path", "old_str", "decl", "start_line", "end_line", "replace", "summary", "type"} {
		a, inTool := tool[field].(map[string]any)
		b, inGrammar := grammar[field].(map[string]any)
		if !inTool || !inGrammar {
			t.Errorf("%q is in the tool=%v and in the grammar=%v", field, inTool, inGrammar)
			continue
		}
		if a["type"] != b["type"] {
			t.Errorf("%q is %v in the tool and %v in the grammar", field, a["type"], b["type"])
		}
	}
	// The grammar names the action; the tool is the action, so it must not.
	if _, ok := tool["action"]; ok {
		t.Error("the tool carries an \"action\" field, which the tool name already says")
	}
}

// ALL THREE WAYS OF SAYING WHERE SURVIVED THE FLATTENING. The array form made
// them a oneOf over three object shapes, which is what the model could not
// close; flat, they are optional siblings and exactly one may be given —
// enforced by edit.Address rather than by the schema. Losing one silently would
// leave an edit that can only be expressed by rewriting a whole file.
func TestTheFlatWriteKeepsEveryWayOfSayingWhere(t *testing.T) {
	var tool map[string]any
	for _, tl := range Tools(ModeDevelop) {
		if tl.Name == ActionWriteFile {
			tool, _ = tl.Parameters["properties"].(map[string]any)
		}
	}
	if tool == nil {
		t.Fatal("the write tool defines no properties")
	}
	for _, want := range []string{"old_str", "decl", "start_line", "end_line"} {
		if _, ok := tool[want]; !ok {
			t.Errorf("the flat write cannot say where by %q", want)
		}
	}
	// AND NONE OF THEM IS REQUIRED, because a whole-file write gives none.
	req := tool["required"]
	_ = req
	for _, tl := range Tools(ModeDevelop) {
		if tl.Name != ActionWriteFile {
			continue
		}
		required, _ := tl.Parameters["required"].([]string)
		for _, must := range required {
			if slices.Contains([]string{"old_str", "decl", "start_line", "end_line"}, must) {
				t.Errorf("%q is required, so a whole-file write is not expressible", must)
			}
		}
	}
}

// A COMMIT OUTSIDE THE ALLOWLIST IS NOT A STYLE PREFERENCE, it is a commit that
// will not land: commitlint rejects it on the commit-msg hook.
func TestTheCommitTypeIsAClosedSetEverywhereItAppears(t *testing.T) {
	fromGrammar, _ := props(t, branchFor(t, ActionWriteFile))["type"].(map[string]any)["enum"].([]string)
	if len(fromGrammar) == 0 {
		t.Fatal("the grammar leaves the commit type open")
	}
	if fmt.Sprint(fromGrammar) != fmt.Sprint(ConventionalTypes) {
		t.Errorf("the grammar's types %v are not the allowlist %v", fromGrammar, ConventionalTypes)
	}
	for _, want := range []string{"feat", "fix", "test", "docs"} {
		if !slices.Contains(ConventionalTypes, want) {
			t.Errorf("the allowlist is missing %q", want)
		}
	}
}

func TestTheGrammarBoundsWhatOneTurnCanMove(t *testing.T) {
	read := props(t, branchFor(t, ActionReadFiles))
	if read["paths"].(map[string]any)["maxItems"] != MaxReadPaths {
		t.Errorf("the read cap is %v, want %d", read["paths"].(map[string]any)["maxItems"], MaxReadPaths)
	}
	if EditsSchema()["maxItems"] != MaxWriteFiles {
		t.Errorf("the write cap is %v, want %d", EditsSchema()["maxItems"], MaxWriteFiles)
	}
	if EditsSchema()["minItems"] != 1 {
		t.Error("the grammar admits a write with no edits in it")
	}
}
