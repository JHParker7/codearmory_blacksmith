package plan

import "strings"

// TaskFileIntro is how a plan names the source file a task's code belongs in.
//
// A CONSTANT SHARED BETWEEN THE STAGE THAT WRITES IT AND THE STAGE THAT READS IT
// BACK. The scoping stage writes this sentence into a task's description; the
// ticket-merge stage cuts on it to recover the file. Coupling two stages by
// retyping one's prose in the other is a bug waiting for someone to reword a
// sentence — naming it is not.
//
// It lives HERE, with the breakdown vocabulary both stages already import,
// rather than in either of them: a producer that has to import its consumer to
// name its own output has the dependency backwards.
const TaskFileIntro = "Put this task's code in `"

// SpecFileFor names the test file for a section, from the source file the
// breakdown assigned it.
//
// One derivation, in one place, because the author is told to write this exact
// name and the developer is told it may not touch it. Two stages computing the
// same name separately is how they come to disagree.
func SpecFileFor(source string) string {
	base := strings.TrimSuffix(source, ".go")
	if base == source {
		return source + "_test.go"
	}
	return base + "_test.go"
}
