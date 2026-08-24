package model

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The gateway's error clip is the transport's, and this pins that it stays so.
//
// Both packages had their own copy and both split multi-byte runes the same way,
// so fixing one would have left the other emitting log lines that are not valid
// text — which is exactly how the version rule came to disagree with itself.
func TestTheErrorSnippetCutsOnARuneBoundary(t *testing.T) {
	for _, r := range []string{"é", "—", "🔥", "日"} {
		got := errorSnippet(strings.NewReader(strings.Repeat(r, 500)))
		if !utf8.ValidString(got) {
			t.Errorf("errorSnippet of repeated %q produced invalid UTF-8", r)
		}
	}
}

func TestTheErrorSnippetNamesAnEmptyBody(t *testing.T) {
	if got := errorSnippet(strings.NewReader("")); got != "<empty body>" {
		t.Errorf("errorSnippet(empty) = %q; a message ending in a bare colon reads as truncated", got)
	}
}
