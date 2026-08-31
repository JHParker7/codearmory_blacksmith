package main

import (
	"strings"
	"testing"
)

// The image reference is the registry, the project, and a run-tagged version —
// sanitised so a project or run name that is not already a legal docker ref
// still yields one.
func TestImageRefIsRegistryProjectRun(t *testing.T) {
	t.Setenv(registryEnvURL, "localhost:5000")
	if got := imageRef("drop-python", "e2e-val"); got != "localhost:5000/drop-python:run-e2e-val" {
		t.Fatalf("imageRef = %q", got)
	}
	// Unsafe characters collapse to dashes; the ref stays legal.
	got := imageRef("My Project/v2", "003 Feature")
	if strings.Contains(got, " ") {
		t.Errorf("ref has a space: %q", got)
	}
	if !strings.HasPrefix(got, "localhost:5000/my-project-v2:run-003-feature") {
		t.Errorf("sanitised ref = %q", got)
	}
	// A trailing slash on the registry URL does not double up.
	t.Setenv(registryEnvURL, "localhost:5000/")
	if got := imageRef("p", "r"); got != "localhost:5000/p:run-r" {
		t.Errorf("trailing-slash ref = %q", got)
	}
}

func TestRegistryURLDefaultsLocal(t *testing.T) {
	t.Setenv(registryEnvURL, "")
	if registryURL() != "localhost:5000" {
		t.Errorf("default registry = %q, want localhost:5000", registryURL())
	}
	t.Setenv(registryEnvURL, "reg.example:5000")
	if registryURL() != "reg.example:5000" {
		t.Errorf("configured registry not honoured: %q", registryURL())
	}
}
