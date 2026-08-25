package transcript

import (
	"context"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/model"
)

// A TRUNCATED REPLY AND A MALFORMED ONE MUST NOT BE THE SAME RECORD.
//
// They need opposite responses: "length" means raise the limit or ask for less,
// anything else means the model got the shape wrong. ChatResult carries the
// reason for exactly that purpose and the transcript dropped it, so the corpus
// could not answer the question it was kept to answer.
//
// Found on a live run: a product manager's split came back "unexpected EOF" —
// the signature of a reply cut off mid-JSON — and nothing in the transcript
// could confirm that rather than leave it to be inferred.
func TestATruncatedTurnSaysItWasTruncated(t *testing.T) {
	sink := &memSink{}
	r := New(sink, "host-1")
	ctx := r.Start(context.Background(), "t-1@host-1", "t-1", "pm-agent")

	r.Turn(ctx, model.ChatRequest{}, model.ChatResult{
		Content:      `{"tasks":[{"title":"half a repl`,
		FinishReason: "length",
	}, nil)

	rec := firstTurn(t, sink)
	if rec.FinishReason != "length" {
		t.Errorf("finish_reason = %q; a reply cut off mid-JSON is indistinguishable "+
			"from a malformed one without it", rec.FinishReason)
	}
}

func TestACompleteTurnSaysSo(t *testing.T) {
	sink := &memSink{}
	r := New(sink, "host-1")
	ctx := r.Start(context.Background(), "t-1@host-1", "t-1", "pm-agent")

	r.Turn(ctx, model.ChatRequest{}, model.ChatResult{
		Content:      `{"tasks":[]}`,
		FinishReason: "stop",
	}, nil)

	if rec := firstTurn(t, sink); rec.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want \"stop\"", rec.FinishReason)
	}
}

// firstTurn is the turn record the recorder wrote.
func firstTurn(t *testing.T, s *memSink) Record {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.records {
		if r.Kind == KindTurn {
			return r
		}
	}
	t.Fatalf("no turn record was written; got %d records", len(s.records))
	return Record{}
}
