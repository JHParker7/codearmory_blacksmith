// Command dumpgate prints one gate's script, so it can be run against a real
// tree rather than against a copy of itself.
//
// SO A CHECK IS VERIFIED AGAINST WHAT SHIPS. Testing a shell gate by
// reimplementing it in Go tests the reimplementation, which is the mistake this
// repository keeps finding in its own fakes.
package main

import (
	"fmt"
	"os"

	"github.com/code-armory-app/blacksmith/internal/gate"
)

func main() {
	which := ""
	if len(os.Args) > 1 {
		which = os.Args[1]
	}
	switch which {
	case "imports":
		fmt.Print(gate.TestImportScript())
	default:
		fmt.Print(gate.FixtureLiteralScript())
	}
}
