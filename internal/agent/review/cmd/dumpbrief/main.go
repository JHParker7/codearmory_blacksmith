// Command dumpbrief prints the reviewer's system prompt.
//
// SO A LIVE CHECK RUNS AGAINST THE BRIEF THAT SHIPS. Asking a model whether it
// would catch something, using a hand-copied prompt, tests the copy — which is
// the mistake this repository keeps finding in its own fakes.
package main

import (
	"fmt"

	"github.com/code-armory-app/blacksmith/internal/agent/review"
)

func main() { fmt.Print(review.SystemPrompt) }
