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
