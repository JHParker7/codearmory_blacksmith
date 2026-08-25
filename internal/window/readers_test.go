package window

import "time"

// readActs and readThoughts ask ReadDigest the two questions the window used to
// have a separate reader for.
//
// THE TESTS GO THROUGH THE PRODUCTION READER ON PURPOSE. There were two readers:
// ReadActivity walked the corpus for activity and ReadThoughts walked it again
// for reasoning, and ReadDigest replaced both with one pass because two passes
// over a 48 MB corpus cost 220 ms and 212 ms PER FRAME — on the event loop.
//
// Keeping the old pair alive for their tests would have been the worse half of
// the trade: dead code that only its tests exercise can drift from the live path
// while every test still passes, which is the exact shape of the bug that let a
// stage run with no system prompt and a retry lose the claim race to itself. One
// reader, and the tests hold it to what the window actually does.
func readActs(dir string, now time.Time) map[string]Activity {
	return ReadDigest(dir, nil, now).Acts
}

func readThoughts(dir, ticketID string) []Thought {
	if ticketID == "" {
		return nil
	}
	return ReadDigest(dir, []string{ticketID}, time.Now()).Thoughts[ticketID]
}
