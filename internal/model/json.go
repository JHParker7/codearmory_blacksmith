package model

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Decoding JSON that a MODEL wrote, which is not the same problem as decoding
// JSON.
//
// Every agent here asks for one JSON object and gets back whatever the model
// produced. The most common way that is not valid JSON is also the most
// mechanical: a raw newline inside a string literal. A model asked to put the
// contents of a Go file into a "content" field writes the file — with real line
// breaks — because that is what the file looks like, and `\n` is an encoding
// detail it drops under any pressure to produce the obvious thing.
//
// Observed: a developer agent spent all eight of its iterations, and the
// ticket's entire attempt budget, in twenty seconds without running a single
// sandbox, because every reply failed to parse this way. The model was not
// confused about the task; it was confused about escaping.
//
// So the invalid character is REPAIRED rather than rejected. The information is
// all there and unambiguous — a control character inside a string can only have
// been meant as itself — and a retry buys nothing, since the next reply is
// written by the same model with the same habit. What is NOT repaired is
// anything ambiguous: an unescaped quote inside a string could end it or belong
// in it, and guessing there would risk silently changing the code the agent is
// about to commit.

// DecodeJSON unmarshals a model's JSON object, repairing raw control
// characters inside string literals if the strict parse fails.
//
// The strict parse is tried FIRST and unchanged, so well-formed replies — the
// majority — take exactly the path they always did.
func DecodeJSON(payload string, into any) error {
	if err := decodeFirstJSONValue(payload, into); err == nil {
		return nil
	} else if repaired := escapeControlCharsInStrings(payload); repaired != payload {
		if err2 := decodeFirstJSONValue(repaired, into); err2 == nil {
			return nil
		}
		// Report the ORIGINAL error. The repaired text is an internal detail, and
		// an error quoting a position in a string the model never wrote is a worse
		// clue than one quoting the reply it did.
		return err
	} else {
		return err
	}
}

// decodeFirstJSONValue decodes the FIRST complete JSON value in the payload and
// ignores whatever follows it.
//
// WHAT COMES AFTER A COMPLETE OBJECT IS NOT THE OBJECT'S PROBLEM. A model that
// has finished writing its answer and then adds one more character has still
// answered; discarding the whole reply over the character is throwing away a
// design document to punish a typo. Measured: an architect returned 4,135 bytes
// of correct JSON ending `"}]}}` — one closing brace too many — and the stage
// reported "unparseable model output", so the project got no ARCHITECTURE.md and
// every developer after it worked without a shared picture of the system.
//
// json.Decoder is what draws the line: it reads exactly one value and stops,
// where Unmarshal insists the input contain nothing else. A TRUNCATED value
// still fails here, which is the case that genuinely cannot be recovered.
func decodeFirstJSONValue(payload string, into any) error {
	return json.NewDecoder(strings.NewReader(payload)).Decode(into)
}

// escapeControlCharsInStrings escapes raw control characters that appear inside
// JSON string literals, leaving everything outside them — including the
// whitespace that formats the document — exactly as it was.
//
// It tracks string boundaries itself rather than using a regexp: whether a quote
// opens or closes a string depends on the backslashes before it, which is
// precisely the thing a regexp cannot see.
func escapeControlCharsInStrings(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inString := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !inString {
			b.WriteByte(c)
			if c == '"' {
				inString = true
			}
			continue
		}
		switch {
		case c == '\\':
			// An escape sequence is copied whole, so the quote in \" is never read
			// as the end of the string.
			//
			// UNLESS IT IS NOT AN ESCAPE SEQUENCE. A model writing source code into
			// a JSON string produces backslashes that JSON does not recognise — a
			// regex \d, a stray \ before a space — and one of those invalidates the
			// whole object, so a reply carrying a perfectly good file is thrown away
			// for a character in it. Observed live: "invalid character ' ' in string
			// escape code" killed a developer attempt four replies running.
			//
			// The unknown escape is PRESERVED as a literal backslash rather than
			// dropped. Dropping is tempting — a stray \ before a space was probably
			// meant as a space — but it silently rewrites \d to d, corrupting a
			// regex in code about to be committed. Preserving can leave a literal
			// backslash where none was wanted, which fails to compile and is a
			// failure the agent can see and fix. Visible beats silent.
			if i+1 >= len(s) {
				b.WriteString(`\\`)
				continue
			}
			// \u IS ONLY AN ESCAPE IF FOUR HEX DIGITS FOLLOW IT. Accepting it
			// without looking left the one case this function exists to prevent:
			// a reply carrying a good file thrown away for a character in it.
			// Measured directly — "path C:\users\bin" and "see \usr for it" both
			// came back as "invalid character 's' in \u hexadecimal character
			// escape", with the whole edit lost, while "valid \u00e9 here" was fine.
			//
			// A bad \u now takes the same route as any other unknown escape: kept
			// as a literal backslash, because preserving beats dropping and visible
			// beats silent, which is what the rest of this branch already does.
			if isJSONEscape(s[i+1]) && (s[i+1] != 'u' || hasFourHexDigits(s[i+2:])) {
				b.WriteByte(c)
				i++
				b.WriteByte(s[i])
				continue
			}
			b.WriteString(`\\`)
		case c == '"':
			b.WriteByte(c)
			inString = false
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20:
			fmt.Fprintf(&b, `\u%04x`, c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// isJSONEscape reports whether c may follow a backslash in a JSON string.
//
// \u is included here, but the caller checks the four hex digits that must
// follow it before treating the pair as an escape — see hasFourHexDigits.
func isJSONEscape(c byte) bool {
	switch c {
	case '"', '\\', '/', 'b', 'f', 'n', 'r', 't', 'u':
		return true
	}
	return false
}

// hasFourHexDigits reports whether s opens with the four hex digits a \u escape
// requires. Anything else means the backslash was not starting an escape at all.
func hasFourHexDigits(s string) bool {
	if len(s) < 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}
