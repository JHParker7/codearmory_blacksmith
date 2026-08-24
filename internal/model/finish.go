package model

// The finish reasons an OpenAI-compatible backend reports.
//
// A CLOSED SET, and anything outside it is treated as neither: a backend that
// invents a word must not be read as saying the reply was complete.
const (
	FinishStop   = "stop"
	FinishLength = "length"
)

// Truncated reports whether a reply stopped because it ran out of room rather
// than because it was finished.
//
// TWO SIGNALS, EITHER SUFFICIENT, because neither is reliable alone. Some
// backends report finish_reason faithfully and some omit it entirely; the token
// count catches the second case, and the finish reason catches a reply that
// stopped at a ceiling this caller did not set — a server-side max, or a
// context window reached before the ceiling was.
//
// It matters because TRUNCATION AND MALFORMEDNESS READ THE SAME AND ARE NOT THE
// SAME. A cut-off object and a refusal both fail to parse, and they need
// opposite fixes: "your reply was rejected" makes a model try the same thing
// again, where "you were cut off" makes it write less. A ceiling of 0 means the
// caller set none, so only the reported reason counts.
func (r ChatResult) Truncated(ceiling int) bool {
	if r.FinishReason == FinishLength {
		return true
	}
	return ceiling > 0 && r.CompletionTokens >= ceiling
}
