package config

import "testing"

// THE TWO PIPELINE SHAPES ARE OFF BY DEFAULT, which is the shape every run so
// far has used. They exist so the alternative stays runnable on the same seed
// and the answer is a measurement instead of an argument — a default that
// silently changed the shape would make the two incomparable.
func TestThePipelineShapeOptionsDefaultOff(t *testing.T) {
	clean(t, oneClass())
	cfg, _ := Load()
	if cfg.MergeTasksFirst {
		t.Error("tasks are merged by default; a board would carry one generic ticket")
	}
	if cfg.OneSpecAuthorPerTask {
		t.Error("one author per task is the default; that is the shape under test, not the baseline")
	}

	env := oneClass()
	env["AGENTS_PM_ONE_TASK"] = "true"
	env["AGENTS_SPEC_ONE_AUTHOR"] = "true"
	clean(t, env)
	cfg, _ = Load()
	if !cfg.MergeTasksFirst || !cfg.OneSpecAuthorPerTask {
		t.Errorf("an explicit setting was ignored: merge=%v oneAuthor=%v",
			cfg.MergeTasksFirst, cfg.OneSpecAuthorPerTask)
	}

	// Anything that is not the magic word leaves the default alone, so a typo in
	// the env file cannot quietly change the pipeline's shape.
	env["AGENTS_PM_ONE_TASK"] = "yes"
	env["AGENTS_SPEC_ONE_AUTHOR"] = "1"
	clean(t, env)
	cfg, _ = Load()
	if cfg.MergeTasksFirst || cfg.OneSpecAuthorPerTask {
		t.Errorf("an unrecognised value changed the shape: merge=%v oneAuthor=%v",
			cfg.MergeTasksFirst, cfg.OneSpecAuthorPerTask)
	}
}
