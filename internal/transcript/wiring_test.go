package transcript

import (
	"context"
	"testing"
)

// THE RECORDER TRAVELS WITH THE IDENTITY, attached by Start.
//
// This is the test that was missing. The identity rode the context and the
// recorder did not, so every sandbox call in the department passed nil and not
// one action record was ever written — across every stage, for the life of the
// rebuild. The board could say which column a ticket sat in and never what any
// stage was doing, because the activity feed was rendering from a stream with no
// producer.
//
// Found by reading a stuck run's transcript by hand: 119 turns, zero actions,
// zero refusals.
func TestStartPutsTheRecorderOnTheContext(t *testing.T) {
	r := New(&memSink{}, "osiris")
	ctx := r.Start(context.Background(), "tr-1", "task-1", "dev-agent")

	got := RecorderFrom(ctx)
	if got == nil {
		t.Fatal("Start did not attach the recorder; every stage's sandbox calls " +
			"would record nothing and say nothing about it")
	}
	if got != r {
		t.Fatal("a different recorder came back")
	}
	// The identity must still be there — this adds to it rather than replacing it.
	if id := From(ctx); id.TaskID != "task-1" || id.Role != "dev-agent" {
		t.Fatalf("the identity was lost: %+v", id)
	}
}

// A CONTEXT THAT WAS NEVER STARTED IS NOT RECORDING, and asking must be safe
// rather than a panic waiting for the first stage that does not record.
func TestRecorderFromAnUnstartedContext(t *testing.T) {
	if got := RecorderFrom(context.Background()); got != nil {
		t.Fatalf("got %v, want nil for a context with no transcript", got)
	}
	// And the nil is safe to use, because every method guards it. This is what
	// lets a caller pass the result straight through without checking.
	var r *Recorder
	r.Action(context.Background(), "run", "go test", nil)
	r.Finish(context.Background(), "ok", "")
}

// A NIL RECORDER ON A STARTED CONTEXT stays nil rather than becoming a non-nil
// interface wrapping one, which is the shape that panics later.
func TestStartOnANilRecorder(t *testing.T) {
	var r *Recorder
	ctx := r.Start(context.Background(), "tr-1", "task-1", "dev-agent")
	if got := RecorderFrom(ctx); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

// THE ACTIONS A STAGE TAKES REACH THE TRANSCRIPT. End to end, because the two
// halves were each correct on their own and the join was what was missing.
func TestAnActionRecordedThroughTheContextLands(t *testing.T) {
	sink := &memSink{}
	r := New(sink, "osiris")
	ctx := r.Start(context.Background(), "tr-1", "task-1", "spec-agent")

	// Exactly what a stage does: take the recorder off the context and hand it to
	// the sandbox.
	RecorderFrom(ctx).Action(ctx, "run", "git push origin bs/t1", nil)

	var actions []Record
	for _, rec := range sink.all() {
		if rec.Kind == KindAction {
			actions = append(actions, rec)
		}
	}
	if len(actions) != 1 {
		t.Fatalf("got %d action records, want 1", len(actions))
	}
	a := actions[0]
	if a.TaskID != "task-1" {
		t.Errorf("TaskID = %q — an action that cannot be joined to a ticket cannot "+
			"be shown on its row", a.TaskID)
	}
	if a.Role != "spec-agent" {
		t.Errorf("Role = %q, want the stage that acted", a.Role)
	}
	if a.Detail != "git push origin bs/t1" {
		t.Errorf("Detail = %q, want the command", a.Detail)
	}
}
