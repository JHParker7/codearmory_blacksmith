package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/code-armory-app/blacksmith/internal/transport"
)

// A LEASE IS A SANDBOX HELD OPEN ACROSS MANY COMMANDS.
//
// The one-shot model creates and destroys a sandbox per command, which is right
// for a caller running one command and wrong for an agent: a developer task runs
// a survey, several reads and several verify passes against ONE checkout, and
// pays a full microVM boot and a full clone for each. Measured on the agent
// plane that is most of a task's wall time — the commands are seconds, the
// sandboxes around them are not.
//
// With a lease the agent boots one sandbox at the start of a ticket, clones into
// it once, runs every command inside it, and destroys it when the ticket leaves
// the stage. What that buys beyond the boot is the WARM CACHE: the second vet in
// a lease costs a tenth of the first, because the module and build caches survive
// between commands where before every command started from empty.
//
// It is per-TICKET and never shared between tickets. A sandbox that outlived one
// would carry whatever the last did — a dirty tree, a half-applied patch, or the
// consequences of a prompt injection — into the next.

// LeaseSpec describes a sandbox to hold open.
type LeaseSpec struct {
	Image       string            `json:"image"`
	RunnerClass string            `json:"runner_class,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	SecretRefs  map[string]string `json:"secret_refs,omitempty"`

	// Checkout asks forge to clone the repository into the sandbox's working
	// directory ONCE, at boot, rather than per command. This is most of the point.
	Checkout *CheckoutSpec `json:"checkout,omitempty"`

	// Volumes attaches shared workflow volumes (created via forge's create-volume)
	// to the sandbox. A Workdir mount makes the volume the working directory, so a
	// role operates on the SAME checkout every workflow step shares — the clone a
	// prior forge step laid down, the scanner reports it wrote. Each volume must
	// already exist and belong to the caller, which is why the lease is acquired
	// as the run's user, not as blacksmith.
	Volumes []VolumeMount `json:"volumes,omitempty"`

	// IdleTimeout and MaxLifetime bound a lease the agent fails to release. They
	// are a backstop for a crashed agent, not the expected way a lease ends — a
	// held sandbox is reserved memory, so the agent releases explicitly.
	//
	// FORGE CAPS THESE whatever is asked for: 300s idle, 3600s lifetime. Asking
	// for more is not an error and not honoured, so nothing here should be built
	// on the assumption that a longer number took effect.
	IdleTimeout int64 `json:"idle_timeout,omitempty"`
	MaxLifetime int64 `json:"max_lifetime,omitempty"`
}

// Forge's caps on a lease, so a caller can size its work to what it will
// actually get rather than to what it asked for.
const (
	MaxIdleTimeout = 300 * time.Second
	MaxLifetime    = 3600 * time.Second
)

// VolumeMount attaches a forge workflow volume to a sandbox by its LOGICAL
// HANDLE — the (WorkflowID, Name) pair, NOT a resource name. forge derives the
// resource name itself as fv-<hash(workflow_id)>-<name>; create-volume is
// idempotent on that pair and never returns a resource name for the caller to
// carry (measured against the live forge: create-volume's step output is empty,
// so a downstream mount that referenced it attached nothing). WorkflowID is the
// run id every step of one workflow shares; Name is the logical volume name
// (e.g. "workspace"). Workdir makes it the working directory; ReadOnly mounts it
// read-only.
type VolumeMount struct {
	WorkflowID string `json:"workflow_id"`
	Name       string `json:"name"`
	MountPath  string `json:"mount_path,omitempty"`
	Workdir    bool   `json:"workdir,omitempty"`
	ReadOnly   bool   `json:"read_only,omitempty"`
}

// CheckoutSpec is forge's clone configuration for a lease.
type CheckoutSpec struct {
	// Env names the variable holding the clone URL, normally populated by a
	// secret reference so the credential never passes through this process.
	Env string `json:"env,omitempty"`

	// Ref is the branch to check out. Depth 0 means a FULL clone: the agent needs
	// history to branch and merge, so it is not shallow by default here.
	Ref   string `json:"ref,omitempty"`
	Depth *int   `json:"depth,omitempty"`
}

// Lease is forge's view of a held sandbox.
type Lease struct {
	LeaseID string `json:"lease_id"`
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`
}

// Lease statuses.
const (
	LeaseStarting = "starting"
	LeaseReady    = "ready"
	LeaseStopped  = "stopped"
	LeaseFailed   = "failed"
)

// CreateLease asks forge to boot a sandbox. It returns as soon as the lease is
// recorded; the sandbox is still starting, so callers wait with WaitReady.
func (c *Client) CreateLease(ctx context.Context, spec LeaseSpec) (Lease, error) {
	if spec.Image == "" {
		return Lease{}, errors.New("lease: image is required")
	}
	var out Lease
	err := c.http.Do(ctx, transport.Request{
		Method: http.MethodPost, Path: c.path("/leases"), Body: spec, Out: &out,
	})
	return out, err
}

// GetLease reads a lease's current state.
func (c *Client) GetLease(ctx context.Context, id string) (Lease, error) {
	var out Lease
	err := c.http.Do(ctx, transport.Request{
		Method: http.MethodGet, Path: c.path("/leases/" + url.PathEscape(id)), Out: &out,
	})
	return out, err
}

// ReleaseLease destroys the sandbox.
//
// Callers should always do this rather than letting the timeout collect it:
// every second between the last command and the release is a runner class's
// worth of memory nobody is using.
func (c *Client) ReleaseLease(ctx context.Context, id string) error {
	return c.http.Do(ctx, transport.Request{
		Method: http.MethodDelete, Path: c.path("/leases/" + url.PathEscape(id)),
	})
}

// WaitReady polls until the sandbox accepts commands, fails, or the context
// ends. It reuses the execution poll bounds: a lease boots in about the time a
// short execution takes, so the same backoff fits both.
func (c *Client) WaitReady(ctx context.Context, id string) (Lease, error) {
	lo, hi := c.pollBounds()
	for attempt := 0; ; attempt++ {
		lease, err := c.GetLease(ctx, id)
		if err != nil {
			return lease, fmt.Errorf("lease: poll: %w", err)
		}
		switch lease.Status {
		case LeaseReady:
			return lease, nil
		case LeaseFailed, LeaseStopped:
			// Detail carries what forge could not do — most often a clone that
			// failed, which is worth reporting VERBATIM rather than as "not ready".
			// A message naming the symptom and not the cause sends a correct agent
			// to the wrong place.
			return lease, fmt.Errorf("lease %s: %s", lease.Status, lease.Detail)
		}
		select {
		case <-ctx.Done():
			return lease, ctx.Err()
		case <-time.After(pollInterval(attempt, lo, hi)):
		}
	}
}
