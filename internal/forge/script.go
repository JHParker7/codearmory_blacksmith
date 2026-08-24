package forge

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/code-armory-app/blacksmith/internal/transport"
)

// Preamble prepares a writable working directory, and every script this
// department runs starts with it.
//
// Forge runs sandboxes with a READ-ONLY ROOT FILESYSTEM and a single writable
// tmpfs at /tmp — which is correct, and which breaks every tool that assumes it
// can write somewhere. `git clone` into the working directory fails with "could
// not create work tree dir: Read-only file system", and Go then fails separately
// trying to create /.cache/go-build. npm and pip fail the same way for the same
// reason, so HOME is moved too rather than patching one toolchain at a time.
//
// GOPATH is moved for the same reason and was MISSED the first time. GOCACHE and
// GOMODCACHE cover building and downloading, but the module CHECKSUM cache lives
// under $GOPATH/pkg/sumdb, which still pointed at the read-only /go. Everything
// ordinary kept working, and `go run some/tool@latest` — how a sandbox reaches a
// linter or a scanner it does not have — failed with "open
// /go/pkg/sumdb/sum.golang.org/latest: no such file or directory", which reads
// like a corrupt toolchain rather than a read-only mount.
//
// Found by running a real clone-and-test in the deployed sandbox. No unit test
// would have caught it, because a fake has no filesystem.
//
// COMMIT_MSG_FILE lives here too: a commit message is written to that file
// rather than formatted into a `git commit -m` argument, so model-authored text
// never reaches a shell word.
const Preamble = `set -e
export HOME=/tmp
export GOPATH=/tmp/go
export XDG_CACHE_HOME=/tmp/.cache
export GOCACHE=/tmp/.cache/go-build
export GOMODCACHE=/tmp/.cache/go-mod
export npm_config_cache=/tmp/.cache/npm
export COMMIT_MSG_FILE=/tmp/.commit-msg
mkdir -p /tmp/.cache /tmp/work
cd /tmp/work
`

// Quote wraps a string as one POSIX single-quoted shell word.
//
// Interpolating a Go string straight into '...' is a trap that does not look
// like one: it reads correctly and works for every value until one contains an
// APOSTROPHE. It cost a real outage here — an evidence label reading "this
// project's own code" closed the quote early, left the script with an unbalanced
// one, and made the shell exit 2 on a SYNTAX ERROR. Nothing in the script ran, so
// every security review failed with an empty diff and no output to explain it.
// The apostrophe was in prose nobody thought of as code.
//
// The escape is the POSIX one — end the quote, emit an escaped apostrophe, start
// a new quote — because there is no way to escape ' inside single quotes.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// RunnerClass is forge's sizing record for one named runner class.
//
// NOTE THE FIELD NAMES. Forge calls these memory_mb and cpu_millicores. The
// memory_limit_mb and cpu_limit that appear on an EXECUTION are a different
// thing — the limit stamped onto a run after it happened — and reading a class
// through those names reports nothing and invites the conclusion that the class
// is unsized when it is merely sized under another name.
type RunnerClass struct {
	Name          string `json:"name"`
	MemoryMB      int64  `json:"memory_mb"`
	CPUMillicores int64  `json:"cpu_millicores"`
	PidsLimit     int64  `json:"pids_limit"`
	TmpfsMB       int64  `json:"tmpfs_mb"`
	DiskGB        int64  `json:"disk_gb"`
	Backend       string `json:"backend"`
	Enabled       bool   `json:"enabled"`
	Privileged    bool   `json:"privileged"`
}

// EnsureRunnerClass creates or updates a runner class, so the configured sizing
// is the sizing forge actually applies.
//
// This department DECLARES the class rather than assuming it, because forge's
// seeded classes are sized for containers and the sandbox plane runs microVMs.
// Left to the default, a developer task compiles inside a 256 MB guest and is
// OOM-killed partway through — which surfaces as a TEST FAILURE the agent then
// tries to fix in the code, burning its whole iteration budget on a problem that
// is not in the repository at all.
//
// Idempotent: update the class if it exists, create it if it does not.
func (c *Client) EnsureRunnerClass(ctx context.Context, spec RunnerClass) error {
	if spec.Name == "" {
		return errors.New("runner class: name is required")
	}
	path := c.path("/runner-classes/" + url.PathEscape(spec.Name))

	err := c.http.Do(ctx, transport.Request{Method: http.MethodGet, Path: path})
	switch {
	case err == nil:
		return c.http.Do(ctx, transport.Request{Method: http.MethodPut, Path: path, Body: spec})
	case transport.NotFound(err):
		return c.http.Do(ctx, transport.Request{
			Method: http.MethodPost, Path: c.path("/runner-classes"), Body: spec,
		})
	default:
		return err
	}
}
