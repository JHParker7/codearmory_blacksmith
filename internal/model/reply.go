// Package model decodes what a model actually returns.
//
// DECODING JSON A MODEL WROTE IS NOT THE SAME PROBLEM AS DECODING JSON.
//
// Every agent here asks for one JSON object and gets back whatever the model
// produced. The most common way that is not valid JSON is also the most
// mechanical: a raw newline inside a string literal. A model asked to put the
// contents of a Go file into a "content" field writes the file — with real line
// breaks — because that is what a file looks like, and `\n` is an encoding
// detail it drops under any pressure to produce the obvious thing.
//
// Observed: a developer spent all eight of its iterations, and the ticket's
// entire attempt budget, in twenty seconds without running a single sandbox,
// because every reply failed to parse this way. The model was not confused about
// the task; it was confused about escaping.
//
// So an invalid character is REPAIRED rather than rejected. The information is
// all there and unambiguous — a control character inside a string can only have
// been meant as itself — and a retry buys nothing, since the next reply is
// written by the same model with the same habit.
//
// What is NOT repaired is anything AMBIGUOUS. An unescaped quote inside a string
// could end it or belong in it, and guessing there would silently change code
// the agent is about to commit. That line is the whole design of this file: it
// repairs what has one reading and refuses what has two.
package model

import (
	"encoding/json"
	"fmt"
	"strings"
)

// DecodeJSON unmarshals a model's JSON object, repairing raw control characters
// inside string literals if the strict parse fails.
//
// The strict parse is tried FIRST and unchanged, so well-formed replies — the
// majority — take exactly the path they always did, and the repair can only ever
// turn a failure into a success.
func DecodeJSON(payload string, into any) error {
	err := decodeFirstValue(payload, into)
	if err == nil {
		return nil
	}

	repaired := escapeControlCharsInStrings(payload)
	if repaired == payload {
		return err
	}
	if err2 := decodeFirstValue(repaired, into); err2 == nil {
		return nil
	}

	// The ORIGINAL error. The repaired text is an internal detail, and an error
	// quoting a position in a string the model never wrote is a worse clue than
	// one quoting the reply it did.
	return err
}

// decodeFirstValue decodes the FIRST complete JSON value in the payload and
// ignores whatever follows it.
//
// WHAT COMES AFTER A COMPLETE OBJECT IS NOT THE OBJECT'S PROBLEM. A model that
// has finished writing its answer and then adds one more character has still
// answered; discarding the whole reply over that character is throwing away a
// design document to punish a typo. Measured: an architect returned 4,135 bytes
// of correct JSON ending `"}]}}` — one closing brace too many — and the stage
// reported "unparseable model output", so the project got no architecture
// document and every developer after it worked without a shared picture of the
// system.
//
// json.Decoder is what draws the line: it reads exactly one value and stops,
// where json.Unmarshal insists the input contain nothing else. A TRUNCATED value
// still fails here, which is the case that genuinely cannot be recovered.
func decodeFirstValue(payload string, into any) error {
	return json.NewDecoder(strings.NewReader(payload)).Decode(into)
}

// escapeControlCharsInStrings escapes raw control characters appearing inside
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
			i += writeEscape(&b, s[i+1:])
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

// writeEscape handles a backslash inside a string, given what follows it. It
// returns how many extra bytes it consumed, so the caller advances past a real
// escape sequence whole — otherwise the quote in \" reads as the end of the
// string.
//
// A BACKSLASH THAT DOES NOT START AN ESCAPE IS PRESERVED AS A LITERAL ONE. A
// model writing source code into a JSON string produces backslashes JSON does
// not recognise — a regex \d, a stray \ before a space — and one of those
// invalidates the whole object, so a reply carrying a perfectly good file is
// thrown away for a character in it. Observed live: "invalid character ' ' in
// string escape code" killed a developer attempt four replies running.
//
// Dropping the backslash is tempting — a stray \ before a space was probably
// meant as a space — but it silently rewrites \d to d, corrupting a regex in
// code about to be committed. Preserving can leave a literal backslash where
// none was wanted, which fails to COMPILE, and that is a failure the agent can
// see and fix. VISIBLE BEATS SILENT.
func writeEscape(b *strings.Builder, rest string) int {
	if rest == "" {
		// A trailing backslash escapes nothing. Same rule: keep it, visibly.
		b.WriteString(`\\`)
		return 0
	}
	if !startsEscape(rest) {
		b.WriteString(`\\`)
		return 0
	}
	b.WriteByte('\\')
	b.WriteByte(rest[0])
	return 1
}

// startsEscape reports whether rest opens with a character that may follow a
// backslash in a JSON string.
//
// \u IS ONLY AN ESCAPE IF FOUR HEX DIGITS FOLLOW IT, and accepting it without
// looking left in place the exact failure this file exists to prevent: a reply
// carrying a good file thrown away for one character. Measured — "path
// C:\users\bin" and "see \usr for it" both came back as "invalid character 's'
// in \u hexadecimal character escape", the whole edit lost, while "valid \u00e9
// here" was fine.
//
// A bad \u therefore takes the same route as any other unknown escape: kept as a
// literal backslash. Found by reading this function against its own stated
// principle rather than by a run failing, which is the argument for doing that
// deliberately.
//
// THE LIMIT, STATED: a backslash followed by one of the single-letter escapes is
// genuinely ambiguous and JSON's reading wins. `C:\bin` decodes with a BACKSPACE
// in it, not with `\b`, and `C:\temp` loses a tab. Nothing here can tell those
// apart from a model that meant the escape, and inventing a rule — "a letter run
// that looks like a path" — would be guessing at exactly the boundary this
// package refuses to guess at. A Windows path is not this repository's medium;
// a corrupted regex would have been.
func startsEscape(rest string) bool {
	switch rest[0] {
	case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		return true
	case 'u':
		return hasFourHexDigits(rest[1:])
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
