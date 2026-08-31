package main

// Packaging: after a run's code is built and passing, turn it into a container
// image. A DevOps AGENT writes the Dockerfile — that is a judgement about the
// code, and judgement is what agents are for — and then CODED, deterministic
// steps run `docker build` and push the image to the workshop's own registry.
// The split is deliberate: writing a Dockerfile reads the source and decides,
// but building and pushing a tree is mechanical and should be reproducible byte
// for byte, not re-decided by a model each time. More infrastructure-as-code
// (the manifests, the deploy) is the agent's to write later; the build-and-push
// is coded now and stays coded.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/code-armory-app/blacksmith/internal/agents"
)

// registryEnvURL names the workshop's image registry, the same shape as the git
// URL: an operator setting with a local default. localhost because docker build
// and push run on THIS host beside the daemon, and docker trusts a localhost
// registry as insecure without extra configuration.
const registryEnvURL = "AGENTS_WORKSHOP_REGISTRY_URL"

func registryURL() string {
	if u := os.Getenv(registryEnvURL); u != "" {
		return u
	}
	return "localhost:5000"
}

// packageProject turns a finished run into an image. Best-effort and SEPARATE
// from the run's pass/fail: the code is already built and green, so a registry
// that is down, a docker daemon that is missing, or a Dockerfile that would not
// build must not turn a good build into a failed one. It journals what it did
// either way, so the history says whether an image exists.
func packageProject(ctx context.Context, maker agents.Creator, files map[string]string, repoDir, version string) map[string]string {
	project, run := projectAndRun(repoDir)

	// The DevOps agent writes the Dockerfile; its writes journal through the same
	// hook as any stage, so the Dockerfile lands as its own commit.
	agent := maker.DevOps(files)
	out, tree, err := runStage(ctx, func(map[string]string) (*agents.Agent, error) { return agent, nil }, files,
		"Package this project into a container image by writing its Dockerfile and .dockerignore.")
	if err != nil || !out.Passed {
		slog.Warn("packaging: the DevOps stage did not finish", "error", err)
		curGit.mark("package: no Dockerfile written")
		return files
	}
	files = tree
	if _, ok := files["Dockerfile"]; !ok {
		slog.Warn("packaging: the DevOps stage wrote no Dockerfile at the tree root")
		curGit.mark("package: no Dockerfile at root")
		return files
	}
	if err := writeTree(repoDir, files); err != nil {
		slog.Warn("packaging: could not write the tree for the image build", "error", err)
		return files
	}

	tag, err := buildAndPushImage(ctx, repoDir, project, run, version)
	if err != nil {
		slog.Warn("packaging: image build or push failed", "error", err)
		curGit.mark("package: build/push failed: " + firstLineOf(err.Error()))
		return files
	}
	slog.Info("packaging: image pushed", "image", tag)
	curGit.mark("package: pushed " + tag)
	announce("package", tag, 0)
	return files
}

// tagUnsafe matches anything a docker image reference path/tag may not contain,
// so a project or run name with spaces or slashes still yields a legal ref.
var tagUnsafe = regexp.MustCompile(`[^a-z0-9._-]+`)

// imageRef names the image: the registry, the project, and a tag. The tag is
// the semantic-release VERSION when the history produced one (drop-python:v1.2.0
// — the image says exactly what it is), falling back to the run branch's name
// when nothing releasable was tagged.
func imageRef(project, run, version string) string {
	p := strings.Trim(tagUnsafe.ReplaceAllString(strings.ToLower(project), "-"), "-.")
	if p == "" {
		p = "project"
	}
	tag := "run-" + strings.Trim(tagUnsafe.ReplaceAllString(strings.ToLower(run), "-"), "-.")
	if v := strings.Trim(tagUnsafe.ReplaceAllString(strings.ToLower(version), "-"), "-."); v != "" {
		tag = v
	}
	return strings.TrimRight(registryURL(), "/") + "/" + p + ":" + tag
}

// buildAndPushImage runs docker build over the run's tree and pushes the image
// to the workshop registry. Both steps are bounded: the run's context carries
// no deadline, and a docker build that stalls pulling a base image, or a push to
// a wedged registry, would otherwise hang the run the way an unbounded git call
// once did.
func buildAndPushImage(ctx context.Context, dir, project, run, version string) (string, error) {
	ref := imageRef(project, run, version)

	bctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(bctx, "docker", "build", "-t", ref, dir).CombinedOutput(); err != nil {
		return "", fmt.Errorf("docker build: %s", firstLineOf(strings.TrimSpace(string(out))))
	}

	pctx, cancelP := context.WithTimeout(ctx, 5*time.Minute)
	defer cancelP()
	if out, err := exec.CommandContext(pctx, "docker", "push", ref).CombinedOutput(); err != nil {
		return "", fmt.Errorf("docker push %s: %s", ref, firstLineOf(strings.TrimSpace(string(out))))
	}
	return ref, nil
}
