package plan

import "strings"

// SplitCovers reports whether a proposed split still asks for everything the
// unit it replaces asked for.
//
// A SPLIT THAT LOSES A CRITERION LOSES IT PERMANENTLY. The children replace the
// parent outright — the parent's own criteria are never read again — so a
// requirement that appears in none of them is not wrong anywhere. It is absent
// everywhere: no criterion, no test, no code, and a run that reports success.
// That is the r80 failure exactly, one level down from where it was measured,
// and the split prompt is where it is most likely: the model is asked to
// DIVIDE a list, and dropping an item is the cheapest way to satisfy "at most 3
// criteria each".
//
// The prompt already says "do not drop any: every criterion you were given must
// appear in exactly one subtask", in those words. This is here because the
// prompt saying it is not the same as it being true — the same reason
// NotCodeWork is code rather than a sentence.
//
// MATCHING IS EXACT AFTER NORMALISING case, surrounding space and trailing
// punctuation, and nothing more. A looser match would accept a criterion the
// model REWROTE, and a rewritten criterion is the other half of the same
// failure: the developer is bound to whatever the child says, so a paraphrase
// that narrows the requirement is a requirement quietly changed. The split
// prompt asks for division, not for editing.
//
// REFUSING IS FREE. It leaves the unit exactly as it was — whole, buildable,
// and carrying every criterion — where accepting is not reversible.
func SplitCovers(parent Subtask, children []Subtask) bool {
	have := make(map[string]bool, len(children)*MaxCriteriaBeforeSplit)
	for _, c := range children {
		for _, a := range c.Acceptance {
			have[normaliseCriterion(a)] = true
		}
	}
	for _, a := range parent.Acceptance {
		if k := normaliseCriterion(a); k != "" && !have[k] {
			return false
		}
	}
	return true
}

// normaliseCriterion folds the differences that are certainly not a change of
// meaning: case, surrounding space, and a trailing full stop.
func normaliseCriterion(s string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(s), ".;, "))
}
