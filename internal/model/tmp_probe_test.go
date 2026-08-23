package model

import "testing"

// Temporary probe: does an unchecked \u break a reply that is otherwise fine?
func TestProbeBadUnicodeEscape(t *testing.T) {
	for _, raw := range []string{
		`{"action":"write_files","edits":[{"path":"a.go","replace":"path C:\users\bin"}]}`,
		`{"action":"write_files","edits":[{"path":"a.go","replace":"see \usr for it"}]}`,
		`{"action":"write_files","edits":[{"path":"a.go","replace":"valid \u00e9 here"}]}`,
	} {
		var act decodeTarget
		err := DecodeJSON(raw, &act)
		got := ""
		if len(act.Edits) > 0 {
			got = act.Edits[0].Replace
		}
		t.Logf("in : %s", raw)
		t.Logf("out: err=%v replace=%q", err, got)
	}
}
