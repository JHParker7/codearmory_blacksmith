package main

import (
	"testing"

	"github.com/code-armory-app/blacksmith/internal/config"
	"github.com/code-armory-app/blacksmith/internal/forge"
)

// EVERY FIELD OF THE CLASS IS DECLARED, because a field left at its zero value
// is not "leave this alone" — it is a declaration that the class should have
// none of it, and EnsureRunnerClass writes the whole record.
//
// The first version of this omitted TmpfsMB. Forge refused it outright —
// "tmpfs_mb must be at least 16" — and that refusal is the only reason startup
// did not quietly resize the working class to a tmpfs of zero, which is where
// SandboxEnv puts HOME, GOPATH, the build cache and the module cache. Caught on
// a live run, by the department declining to start the very feature that had
// just been added to protect the sizing.
func TestTheDeclaredRunnerClassLeavesNoFieldUnset(t *testing.T) {
	spec := devRunnerClass(config.Config{
		Repo:             config.Repo{RunnerClass: "agent-dev"},
		DevMemoryMB:      8192,
		DevCPUMillicores: 4000,
	})

	for _, c := range []struct {
		name string
		got  int64
		why  string
	}{
		{"MemoryMB", spec.MemoryMB, "a microVM compiling a real toolchain"},
		{"CPUMillicores", spec.CPUMillicores, "a parallel build"},
		{"PidsLimit", spec.PidsLimit, "a compiler forked per core"},
		{"TmpfsMB", spec.TmpfsMB, "HOME, GOPATH and both Go caches live in /tmp"},
		{"DiskGB", spec.DiskGB, "the checkout and its build output"},
	} {
		if c.got <= 0 {
			t.Errorf("%s is %d; forge is being told this class needs none, and %s",
				c.name, c.got, c.why)
		}
	}

	if spec.Name == "" {
		t.Error("the class has no name, so there is nothing to declare")
	}
	if !spec.Enabled {
		t.Error("the class is declared disabled, so every lease against it fails")
	}
}

// THE SIZING COMES FROM THE OPERATOR, not from the defaults, wherever they said
// so — otherwise a host tuned for a bigger repository is silently reset to the
// built-in numbers every time it starts.
func TestTheOperatorsSizingReachesTheDeclaration(t *testing.T) {
	spec := devRunnerClass(config.Config{
		Repo:             config.Repo{RunnerClass: "custom"},
		DevMemoryMB:      16384,
		DevCPUMillicores: 8000,
	})

	if spec.Name != "custom" {
		t.Errorf("class name = %q, want the configured one", spec.Name)
	}
	if spec.MemoryMB != 16384 || spec.CPUMillicores != 8000 {
		t.Errorf("declared %d MB / %d millicores, want the operator's 16384 / 8000",
			spec.MemoryMB, spec.CPUMillicores)
	}
}

var _ = forge.RunnerClass{}
