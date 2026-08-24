package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// The ask line: one field on the board itself, for handing the department work
// without leaving the view you are watching it in.
//
// THE COMPOSE SCREEN ALREADY EXISTED AND IS NOT WHAT THIS REPLACES. That screen
// is for a request you have thought about — a title, a body, room to type. This
// is for the other case, which is most of them: a sentence you already have in
// your head, typed where you are already looking, gone in one keystroke.
//
// A bare sentence is a thin brief, so it goes through the model first — see
// refineRequest. That call is the whole reason this can be one line instead of
// two fields: the person supplies the intent and the model supplies the shape.

// askPrompt turns a sentence into a request the department can scope.
//
// IT MAY NOT ADD REQUIREMENTS. Everything downstream treats the request as the
// authority on what was asked for: the product manager draws acceptance criteria
// from it, the specification author is told that anything the request does not
// state is NOT a requirement, and the developer must satisfy every test written
// from it. A refiner that helpfully adds "and it should be paginated" creates
// work nobody asked for and no one can tell apart from work someone did.
//
// So this restates, it does not design. The test is whether every sentence it
// writes could be read straight off the input.
const askPrompt = `You are turning a short request into a ticket for a software team.

You will be given one sentence or phrase. Produce:
  - a title: a short imperative summary, under 100 characters
  - a request: the same intent written as two or three sentences of plain prose

RESTATE, DO NOT DESIGN. Every statement you write must be something the input
already says or plainly implies. Do NOT add requirements, technologies,
constraints, edge cases, acceptance criteria or scope of your own — a team will
read this as the definitive statement of what was asked for, and anything you
invent becomes work nobody wanted.

If the input is already a complete request, return it almost unchanged.
If the input is vague, keep it vague. Do not resolve the ambiguity by guessing;
a short honest request is better than a detailed wrong one.`

func askSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title": map[string]any{
				"type":        "string",
				"maxLength":   100,
				"description": "A short imperative summary of what was asked for.",
			},
			"request": map[string]any{
				"type":      "string",
				"maxLength": 1200,
				"description": "The same intent as two or three sentences of prose. " +
					"Nothing the input does not already say.",
			},
		},
		"required":             []string{"title", "request"},
		"additionalProperties": false,
	}
}

// refineRequest expands one line into a title and a request body.
//
// THE INPUT IS NEVER LOST. Every failure — no model configured, the call
// erroring, output that will not decode, an empty title — falls back to filing
// the line exactly as typed. A request the person has already written is worth
// more than a better-shaped one they have to type again, and the department
// scopes a thin ticket perfectly well.
func refineRequest(ctx context.Context, gw *Gateway, class Class, line string) (title, body string) {
	line = strings.TrimSpace(line)
	if gw == nil || line == "" {
		return line, ""
	}
	res, err := gw.Chat(ctx, class, ChatRequest{
		Messages: []Message{
			{Role: "system", Content: askPrompt},
			{Role: "user", Content: line},
		},
		// Greedy: the same sentence should produce the same ticket twice, so a
		// person who retypes a request does not get a different one.
		Temperature: 0,
		MaxTokens:   maxLargeReplyTokens,
		Schema:      &ReplySchema{Name: "request", Schema: askSchema()},
	})
	if err != nil {
		return line, ""
	}
	var out struct {
		Title   string `json:"title"`
		Request string `json:"request"`
	}
	if err := decodeModelJSON(res.Content, &out); err != nil {
		return line, ""
	}
	if strings.TrimSpace(out.Title) == "" {
		return line, ""
	}
	// The typed line is kept under the refined prose, because the refiner is the
	// only stage here nobody reviews: if it has quietly changed the meaning, the
	// original is on the ticket to be read against it.
	body = strings.TrimSpace(out.Request)
	if body != "" {
		body += "\n\n"
	}
	body += "Asked for as: " + line
	return strings.TrimSpace(out.Title), body
}

// fileAsk refines the typed line and files it, as one background command.
func (m tuiModel) fileAsk(line string) tea.Cmd {
	board := m.composeBoard()
	if board == "" {
		return func() tea.Msg { return noteMsg("pick a single project with p before filing a request") }
	}
	api, gw, class := m.api, m.gw, m.cfg.PMClass
	return func() tea.Msg {
		// Longer than the compose path's twenty seconds because this one may make
		// a model call first, and a request lost to a timeout is the one outcome
		// worth spending seconds to avoid.
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		title, body := refineRequest(ctx, gw, class, line)
		t := Ticket{Title: title, Description: body, Priority: "medium", Status: ColInbox}
		t.BoardID = &board

		created, err := api.CreateTicket(ctx, t)
		if err != nil {
			return noteMsg("could not file it: " + err.Error())
		}
		return noteMsg(fmt.Sprintf("filed %s — %s", shortID(created.TicketID), clip(title, 60)))
	}
}

// askView is the line itself, drawn under the board.
func (m tuiModel) askView() string {
	if m.asking {
		return cPrompt.Render("  ask  ") + m.ask + cPrompt.Render("█")
	}
	if m.filing {
		return cDim.Render("  ask  filing…")
	}
	return cDim.Render("  ask  / to hand the department something")
}
