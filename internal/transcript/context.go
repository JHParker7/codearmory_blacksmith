package transcript

import "context"

// Identity is which transcript a context belongs to.
//
// CARRIED ON THE CONTEXT rather than threaded through every signature, because
// the gateway records a turn several layers below the agent that started it. The
// alternative is a recorder parameter on every function between them, and the
// first one that forgets to pass it records nothing — silently, which is the
// failure this package exists to prevent.
type Identity struct {
	TranscriptID string
	TaskID       string
	Role         string
}

type key struct{}

// With attaches transcript identity to a context.
func With(ctx context.Context, transcriptID, taskID, role string) context.Context {
	return context.WithValue(ctx, key{}, Identity{
		TranscriptID: transcriptID, TaskID: taskID, Role: role,
	})
}

// From reads the identity back. The zero value means "not in a transcript",
// which every recording method treats as nothing to do.
func From(ctx context.Context) Identity {
	id, _ := ctx.Value(key{}).(Identity)
	return id
}

// ID reports the transcript a context belongs to, or "" if none.
func ID(ctx context.Context) string { return From(ctx).TranscriptID }

// recorderKey carries the RECORDER, as distinct from the identity above.
//
// THE IDENTITY ALONE WAS NOT ENOUGH, and the gap was silent in exactly the way
// this file's opening comment warns about. Every sandbox call takes a recorder
// argument, every caller passed nil, and nothing anywhere called an attach —
// so across the whole department not one action record was ever written. The
// board could say which column a ticket was in and never what a stage was
// doing, because DescribeAction was rendering from a stream with no producer.
//
// Found by reading a stuck run's transcript: 119 turns, zero actions, zero
// refusals.
type recorderKey struct{}

// WithRecorder attaches the recorder to a context.
//
// Start does this for every stage, which is the point: a recorder that has to
// be attached by each caller is one that some caller will not attach.
func WithRecorder(ctx context.Context, r *Recorder) context.Context {
	return context.WithValue(ctx, recorderKey{}, r)
}

// RecorderFrom reads the recorder back. Nil means "not recording", which every
// method on Recorder already treats as nothing to do — so the result is safe to
// pass straight to anything taking one.
func RecorderFrom(ctx context.Context) *Recorder {
	r, _ := ctx.Value(recorderKey{}).(*Recorder)
	return r
}
