package dev

import (
	"github.com/code-armory-app/blacksmith/internal/model"
)

// Schema constrains the reply on a backend with no tool support.
//
// ONE SHAPE PER ACTION, because a single object with everything optional is not
// a constraint at all. This was one flat object requiring only "action", which
// was harmless while it was a fallback and became the whole contract the moment
// the turn stopped offering tools. The tool definitions required edits, summary
// and type on write_files; this copy required none of them, so the grammar
// happily admitted {"action":"write_files"} — 25 refusals of "write_files with
// no edits" in the first window after the switch, every one schema-valid.
//
// EVERY FIELD IS BOUNDED, because this becomes a GRAMMAR and an unbounded string
// in a grammar is an unbounded reply. Measured when a backend without tool
// support first fell through to here: the model filled "type" with
// "replace_all_content_in_file_if_exists_…" and kept going for 5,000 tokens and
// 348 seconds — schema-valid the whole way, because nothing said how long a
// string may be.
func Schema() *model.ReplySchema {
	only := func(action string) map[string]any {
		return map[string]any{"type": "string", "enum": []string{action}}
	}
	return &model.ReplySchema{
		Name: "dev_action",
		Schema: map[string]any{
			"oneOf": []any{
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"action": only(ActionReadFiles),
						"paths": map[string]any{
							"type":     "array",
							"items":    map[string]any{"type": "string", "maxLength": MaxPathChars},
							"maxItems": MaxReadPaths,
							"minItems": 1,
						},
					},
					"required":             []string{"action", "paths"},
					"additionalProperties": false,
				},
				// FLAT HERE TOO, for the reason it is flat in the tool form: an array
				// of objects each carrying a file is a shape the model closes wrongly
				// at depth, and the reply schema is the other place it could learn to
				// build one. The array form is still ACCEPTED — see ActionWriteFiles —
				// it is simply not what anything asks for.
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"action":     only(ActionWriteFile),
						"path":       map[string]any{"type": "string", "maxLength": MaxPathChars},
						"old_str":    map[string]any{"type": "string", "maxLength": MaxAnchorChars},
						"decl":       map[string]any{"type": "string", "maxLength": MaxDeclChars},
						"start_line": map[string]any{"type": "integer", "minimum": 0},
						"end_line":   map[string]any{"type": "integer", "minimum": 0},
						"replace":    map[string]any{"type": "string"},
						"summary":    map[string]any{"type": "string", "maxLength": MaxSummaryRunes},
						"type":       map[string]any{"type": "string", "enum": ConventionalTypes},
					},
					// AN EDIT-LESS WRITE IS NOT A SMALLER EDIT, IT IS A WASTED TURN.
					"required":             []string{"action", "path", "replace", "summary", "type"},
					"additionalProperties": false,
				},
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"action": only(ActionUndoEdit),
						"reason": map[string]any{"type": "string", "maxLength": MaxSummaryRunes},
					},
					"required":             []string{"action"},
					"additionalProperties": false,
				},
			},
		},
	}
}

// Tools offers the loop's actions as callable functions.
//
// ONE TOOL PER ACTION, rather than one tool with an action enum, because that is
// what the models are trained on and it lets each action carry only its own
// arguments — "edits" is required on write_files and cannot be omitted, which
// was previously a silent empty write.
//
// FILTERED THROUGH Actions so the offered tools and the accepted ones cannot
// drift apart. run_tests, finish and give_up still have definitions here and are
// simply not listed: keeping them makes restoring one a one-line change, whereas
// a tool offered but not accepted is a trap of exactly the kind this removes.
func Tools(mode Mode) []model.Tool {
	all := []model.Tool{
		{
			Name: ActionReadFiles,
			Description: "Read files from the repository. Name every file you need in ONE call — " +
				"each call costs an iteration and you have few.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"paths": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Repository-relative paths, up to 12.",
					},
				},
				"required":             []string{"paths"},
				"additionalProperties": false,
			},
		},
		{
			Name: ActionWriteFile,
			Description: "Edit ONE file. " + mode.EditRule() +
				` Say WHERE in exactly one way: put the exact snippet you are replacing in ` +
				`"old_str" (it must appear exactly once), OR name a whole function or type in ` +
				`"decl", OR give "start_line"/"end_line" to pick one of several identical lines. ` +
				`To write a whole file, give "path" and "replace" and leave the others out. ` +
				`The new text always goes in "replace". NEVER put the same text in old_str and ` +
				"replace — old_str is what is there now, replace is what it becomes. " +
				"What you do not name, you do not change. Call this again for the next file.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
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
						"description": `The declaration to replace whole: "main", "apiTasksHandler", "Store.Add".`,
					},
					"start_line": map[string]any{"type": "integer", "minimum": 0},
					"end_line":   map[string]any{"type": "integer", "minimum": 0},
					"replace": map[string]any{
						"type":        "string",
						"description": "The new text. This is the only place new code goes.",
					},
					"summary": map[string]any{"type": "string", "description": "One line describing the change."},
					"type": map[string]any{
						"type":        "string",
						"enum":        ConventionalTypes,
						"description": "Conventional Commits type for the commit message.",
					},
				},
				"required":             []string{"path", "replace", "summary", "type"},
				"additionalProperties": false,
			},
		},
		{
			Name: ActionUndoEdit,
			Description: "Put the file back to what it was before your last write. Use this the moment " +
				"an edit leaves a file you no longer recognise — once the text on disk has diverged " +
				"from what you expect, neither old_str nor a line number will find what you are " +
				"looking for, and further edits make it worse. Undo, read the file, then try again.",
			Parameters: emptyArgs(),
		},
		{
			Name:        ActionRunTests,
			Description: mode.CheckDescription(),
			Parameters:  emptyArgs(),
		},
		{
			Name: ActionFinish,
			Description: "Not offered: the stage ends by itself when its checks pass. Kept so restoring " +
				"it is a one-line change to Actions.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"summary": map[string]any{"type": "string", "description": "One line describing the change."},
					"type":    map[string]any{"type": "string", "enum": ConventionalTypes},
				},
				"required":             []string{"summary", "type"},
				"additionalProperties": false,
			},
		},
		{
			Name:        ActionGiveUp,
			Description: "Stop, explaining why this ticket cannot be done. Use this rather than guessing.",
			Parameters: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"reason": map[string]any{"type": "string"}},
				"required":             []string{"reason"},
				"additionalProperties": false,
			},
		},
	}

	allowed := ActionsFor(mode)
	out := make([]model.Tool, 0, len(all))
	for _, t := range all {
		for _, name := range allowed {
			if t.Name == name {
				out = append(out, t)
				break
			}
		}
	}
	return out
}

func emptyArgs() map[string]any {
	return map[string]any{
		"type": "object", "properties": map[string]any{}, "additionalProperties": false,
	}
}
