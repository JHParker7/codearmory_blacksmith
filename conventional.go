package main

// Conventional Commits for the run journal.
//
// The history a run writes is not just a record — it is the INPUT to semantic
// versioning, which reads the commit subjects to decide the next release. So
// every commit must be machine-readable: `type(scope): subject`, the Angular
// convention. Blacksmith already tags each landed edit with a type and summary;
// here we add the scope from the file it touched, and normalise the journal's
// own boundary marks so a commit-msg hook can enforce the format on humans and
// on blacksmith alike without the journal ever tripping it.

import (
	"path"
	"regexp"
	"strings"
)

// ccSubject matches a valid Conventional Commit subject: one of the Angular
// types, an optional (scope), an optional ! for a breaking change, then ": " and
// text. The same set the installed commit-msg hook enforces.
var ccSubject = regexp.MustCompile(`^(feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert)(\([\w./-]+\))?(!)?: .+`)

// ccTyped splits a "type: subject" or "type(scope): subject" message so a scope
// can be injected when one is missing.
var ccTyped = regexp.MustCompile(`^(\w+)(\([\w./-]+\))?(!)?: (.*)$`)

// ccScope derives a commit scope from the path an edit touched: the file's base
// name without its extension (store.go → store), which reads as the component
// changed. Empty for a path with no usable name.
func ccScope(p string) string {
	base := path.Base(p)
	if ext := path.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	base = strings.ToLower(strings.TrimSpace(base))
	if base == "" || base == "." || base == "/" {
		return ""
	}
	return base
}

// ccWithScope injects a scope into a "type: subject" message that has none, so
// the developer's `fix: cap the body` becomes `fix(handlers): cap the body`. A
// message that already carries a scope, or does not parse, is left for
// ccNormalize to guarantee.
func ccWithScope(msg, scope string) string {
	m := ccTyped.FindStringSubmatch(oneLine(msg))
	if m == nil || scope == "" {
		return msg
	}
	if m[2] != "" { // already scoped
		return msg
	}
	return m[1] + "(" + scope + ")" + m[3] + ": " + m[4]
}

// ccNormalize guarantees a subject the commit-msg hook accepts. A message that
// is already conventional passes through; anything else — a boundary mark like
// "run: passed", free text — becomes a chore, its original text kept as the
// subject so the history still reads plainly.
func ccNormalize(msg string) string {
	one := oneLine(msg)
	if ccSubject.MatchString(one) {
		return one
	}
	return "chore: " + one
}

// oneLine collapses a message to a single trimmed line, because a commit subject
// is one line and a multi-line request or mark must not become a multi-line
// subject the hook then rejects.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// commitMsgHook is the commit-msg hook blacksmith installs into a run's
// repository, enforcing the same format ccSubject describes. Installed so the
// project's history stays machine-readable for versioning — for any human who
// clones and commits, and as a backstop on blacksmith's own commits.
const commitMsgHook = `#!/bin/sh
# Installed by blacksmith: enforce Conventional Commits so this project's history
# is machine-readable for semantic-release versioning.
subject=$(head -n1 "$1")
case "$subject" in
	"Merge "*|"fixup! "*|"squash! "*|"Revert "*) exit 0 ;;
esac
if printf '%s' "$subject" | grep -Eq '^(feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert)(\([a-z0-9._/-]+\))?!?: .+'; then
	exit 0
fi
echo "commit rejected: subject must be Conventional Commits — type(scope): subject" >&2
echo "  e.g. feat(store): add owner field    fix(handlers): cap the request body" >&2
echo "  got: $subject" >&2
exit 1
`
