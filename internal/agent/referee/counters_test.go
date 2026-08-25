package referee

import (
	"context"
	"testing"

	"github.com/code-armory-app/blacksmith/internal/model"
	"github.com/code-armory-app/blacksmith/internal/ticket"
)

type counted struct{ owner, confidence []string }

func (c *counted) RefereeVerdict(owner, confidence string) {
	c.owner = append(c.owner, owner)
	c.confidence = append(c.confidence, confidence)
}

// THE REFEREE'S VALUE IS ENTIRELY IN WHETHER IT IS RIGHT, and neither of its
// failure modes shows up in a log line. One that never blames the spec is dead
// weight; one that blames it constantly is sending healthy specifications back,
// spending an author's repair budget and reaching a person as "cannot be
// satisfied" about tests that could be. Only the counter separates them.
func TestAVerdictIsCounted(t *testing.T) {
	c := &counted{}
	r := New(&gateway{content: `{"owner":"spec","reason":"the test writes ` +
		`to a local store","confidence":"high"}`}, model.ClassLarge).WithCounters(c)

	v := r.Judge(context.Background(), nil, ticket.Ticket{Title: "t"},
		map[string]string{
			"store_test.go": "package main\nfunc TestX(t *testing.T) {}\n",
			"store.go":      "package main\n",
		}, "--- FAIL: TestX")

	if v == nil {
		t.Fatal("no verdict came back")
	}
	if len(c.owner) != 1 {
		t.Fatalf("the verdict was counted %d times, want once", len(c.owner))
	}
	if c.owner[0] != OwnerSpec || c.confidence[0] != "high" {
		t.Errorf("counted owner=%q confidence=%q, want the verdict's own values",
			c.owner[0], c.confidence[0])
	}
}

// A REFEREE THAT DECLINED TO RULE IS NOT A REFEREE THAT WAS NEVER ASKED, and
// only the counter can tell them apart afterwards. Without this, a referee
// silently refusing every case is indistinguishable from one nothing consults.
func TestDecliningToRuleIsAlsoCounted(t *testing.T) {
	c := &counted{}
	r := New(&gateway{content: `not json at all`},
		model.ClassLarge).WithCounters(c)

	v := r.Judge(context.Background(), nil, ticket.Ticket{Title: "t"},
		map[string]string{
			"store_test.go": "package main\n",
			"store.go":      "package main\n",
		}, "--- FAIL: TestX")

	if v != nil {
		t.Fatalf("an unreadable reply produced a verdict: %+v", v)
	}
	if len(c.owner) != 1 || c.owner[0] != "none" {
		t.Errorf("a declined ruling was counted as %v, want one \"none\"", c.owner)
	}
}

// NIL COUNTERS ARE A VALID CONFIGURATION: a host that exports no metrics still
// referees.
func TestARefereeWithoutCountersStillRules(t *testing.T) {
	r := New(&gateway{content: `{"owner":"dev","reason":"x","confidence":"high"}`},
		model.ClassLarge)

	v := r.Judge(context.Background(), nil, ticket.Ticket{Title: "t"},
		map[string]string{"a_test.go": "package main\n", "a.go": "package main\n"},
		"--- FAIL")
	if v == nil || v.Owner != OwnerDev {
		t.Fatalf("verdict = %+v, want the developer blamed", v)
	}
}
