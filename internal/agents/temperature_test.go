package agents

import (
	"context"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/tools"
)

// A model repeating the same unproductive move at low temperature repeats it
// forever: the most probable continuation is the one it just tried. Heat rises
// with idleness — mirroring the department's dev.Temperature — and progress
// resets it through the idle counter.
func TestTemperatureRisesWithIdlenessAndResetsOnProgress(t *testing.T) {
	if got := stuckTemperature(0.2, 0); got != 0.2 {
		t.Fatalf("temperature moved with nothing wrong: %v", got)
	}
	if got := stuckTemperature(0.2, 4); got <= 0.2 {
		t.Fatalf("four idle turns did not raise the temperature: %v", got)
	}
	if got := stuckTemperature(0.2, 100); got > 1.0 {
		t.Fatalf("the temperature escaped its ceiling: %v", got)
	}

	// And through the loop: idle turns heat the requests, a landed write cools
	// them back to base.
	o := checked()
	o.Guard = tools.AllowAll
	o.Temperature = 0.2
	o.MaxIterations = 6
	gw := &fakeGateway{replies: []model.ChatResult{
		calls(tools.ReadFiles, `{"paths":["a.go"]}`),
		calls(tools.ReadFiles, `{"paths":["a.go"]}`),
		calls(tools.ReadFiles, `{"paths":["a.go"]}`),
		calls(tools.ReadFiles, `{"paths":["a.go"]}`),
		calls(tools.WriteFile, `{"path":"b.md","replace":"b\n","summary":"x","type":"docs"}`),
		calls(tools.ReadFiles, `{"paths":["a.go"]}`),
	}}
	a := Creator{Gateway: gw, Sandbox: fakeSandbox{exit: 1}}.
		New(map[string]string{"a.go": "package p\n"}, o)

	if _, err := a.Run(context.Background(), "work"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if gw.seen[0].Temperature != 0.2 {
		t.Fatalf("turn 1 temperature = %v, want the base", gw.seen[0].Temperature)
	}
	if gw.seen[4].Temperature <= gw.seen[1].Temperature {
		t.Fatalf("idleness did not heat the requests: turn5=%v turn2=%v",
			gw.seen[4].Temperature, gw.seen[1].Temperature)
	}
	// The write on turn 5 reset idle; turn 6 is back at base.
	if gw.seen[5].Temperature != 0.2 {
		t.Fatalf("a landed write did not cool the sampling: %v", gw.seen[5].Temperature)
	}
}
