package forge

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestWaitReadyWaitsForTheSandboxToBoot(t *testing.T) {
	f, c := newFakeForge(t)
	f.leaseSteps = []string{LeaseStarting, LeaseStarting, LeaseReady}

	lease, err := c.CreateLease(context.Background(), LeaseSpec{Image: "img"})
	if err != nil {
		t.Fatalf("CreateLease: %v", err)
	}
	// It returns as soon as the lease is RECORDED; the sandbox is still starting.
	if lease.Status == LeaseReady {
		t.Error("CreateLease reported a sandbox that cannot yet accept commands")
	}

	ready, err := c.WaitReady(context.Background(), lease.LeaseID)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if ready.Status != LeaseReady {
		t.Errorf("status = %q", ready.Status)
	}
	if f.leasePolls < 3 {
		t.Errorf("polled %d times; it did not wait for the boot", f.leasePolls)
	}
}

// DETAIL CARRIES WHAT FORGE COULD NOT DO — most often a clone that failed. A
// message naming the symptom and not the cause sends a correct agent to the
// wrong place, which is this repository's most expensive failure class.
func TestAFailedLeaseReportsWhatForgeSaid(t *testing.T) {
	f, c := newFakeForge(t)
	f.leaseSteps = []string{LeaseStarting, LeaseFailed}
	f.leaseDetail = "clone failed: authentication required"

	_, err := c.WaitReady(context.Background(), "l-1")
	if err == nil {
		t.Fatal("a failed lease reported ready")
	}
	if !strings.Contains(err.Error(), "authentication required") {
		t.Errorf("err = %v, want forge's own detail", err)
	}
}

// A lease that stops before it is ready is as unusable as one that failed, and
// waiting for it would poll until the context ends.
func TestAStoppedLeaseIsNotWaitedOnForever(t *testing.T) {
	f, c := newFakeForge(t)
	f.leaseSteps = []string{LeaseStopped}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := c.WaitReady(ctx, "l-1"); err == nil {
		t.Error("a stopped lease reported ready")
	} else if ctx.Err() != nil {
		t.Error("WaitReady polled a stopped lease until the deadline")
	}
}

func TestCreateLeaseRefusesASandboxItCannotBoot(t *testing.T) {
	_, c := newFakeForge(t)
	if _, err := c.CreateLease(context.Background(), LeaseSpec{}); err == nil {
		t.Error("a lease with no image was created")
	}
}

// A lease is per-TICKET: the checkout is asked for ONCE, at boot, which is most
// of the point. This pins that the spec reaches forge intact rather than being
// quietly dropped.
func TestTheCheckoutIsPartOfTheLease(t *testing.T) {
	f, c := newFakeForge(t)
	_, err := c.CreateLease(context.Background(), LeaseSpec{
		Image:    "img",
		Checkout: &CheckoutSpec{Env: "GIT_URL", Ref: "agent/t-1"},
		Env:      map[string]string{"CI": "1"},
	})
	if err != nil {
		t.Fatalf("CreateLease: %v", err)
	}
	if len(f.created) != 1 {
		t.Fatalf("forge received %d leases", len(f.created))
	}
	got := f.created[0]
	if got.Checkout == nil || got.Checkout.Ref != "agent/t-1" || got.Checkout.Env != "GIT_URL" {
		t.Errorf("checkout = %+v", got.Checkout)
	}
	// Depth nil means a FULL clone: the agent needs history to branch and merge.
	if got.Checkout.Depth != nil {
		t.Errorf("depth = %v, want a full clone", *got.Checkout.Depth)
	}
}
