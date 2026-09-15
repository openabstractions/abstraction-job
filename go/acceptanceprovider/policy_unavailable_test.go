package acceptanceprovider

import (
	"testing"

	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

// frames runs generated client frames through the provider's policy dispatch.
type frames struct {
	p      *Provider
	scope  string
	permit func(string, string) access
}

func (f frames) ExchangeFrame(frame []byte) ([]byte, error) {
	return f.p.dispatchFrame(frame, f.scope, f.permit)
}

func granting(a access) func(string, string) access {
	return func(string, string) access { return a }
}

func TestPolicyUnavailableObservationResultAndInventory(t *testing.T) {
	p := openTest(t, t.TempDir())
	s := submission(p, "observed-outage")
	accept(t, api.NewRecoverableAcceptanceClient(frames{p, "alice", granting(accessAllowed)}), s)

	outage := frames{p, "alice", granting(accessUnavailable)}
	if v, err := api.NewOperationControlClient(outage).ObserveWork(s.Identity); err != nil || v.Outcome != "unavailable" || v.Snapshot != nil {
		t.Fatalf("observe during outage: %+v %v", v, err)
	}
	if v, err := api.NewOperationControlClient(outage).ReadResult(s.Identity, 0, 16); err != nil || v.Outcome != "unavailable" || v.Chunk != nil {
		t.Fatalf("read during outage: %+v %v", v, err)
	}
	if v, err := api.NewJobInventoryClient(outage).ListWork("", 8); err != nil || v.Outcome != "unavailable" || len(v.Snapshots) != 0 || v.Next != "" || v.Complete {
		t.Fatalf("inventory during outage: %+v %v", v, err)
	}

	denied := frames{p, "alice", granting(accessDenied)}
	if v, err := api.NewOperationControlClient(denied).ObserveWork(s.Identity); err != nil || v.Outcome != "forbidden" {
		t.Fatalf("denied observe: %+v %v", v, err)
	}
	if v, err := api.NewJobInventoryClient(denied).ListWork("", 8); err != nil || v.Outcome != "forbidden" {
		t.Fatalf("denied inventory: %+v %v", v, err)
	}
	allowed := frames{p, "alice", granting(accessAllowed)}
	if v, err := api.NewOperationControlClient(allowed).ObserveWork(s.Identity); err != nil || v.Outcome != "observed" {
		t.Fatalf("observe after outage: %+v %v", v, err)
	}
}

func TestPolicyUnavailableLeavesNoEffect(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	s := submission(p, "outage")
	outage := api.NewRecoverableAcceptanceClient(frames{p, "alice", granting(accessUnavailable)})

	v, err := outage.Submit(s)
	if err != nil || v.Outcome != "unavailable" || v.Receipt != nil {
		t.Fatalf("submit during outage: %+v %v", v, err)
	}
	if v, err := outage.Reconcile(s.Identity); err != nil || v.Outcome != "unavailable" {
		t.Fatalf("reconcile during outage: %+v %v", v, err)
	}
	if v, err := outage.CancelWork(s.Identity); err != nil || v.Outcome != "unavailable" {
		t.Fatalf("cancel during outage: %+v %v", v, err)
	}
	if _, err := outage.GetHistoryWindow(); err == nil {
		t.Fatal("history window granted during outage")
	}
	if n := jobs(t, root); n != 0 {
		t.Fatalf("outage created work: %d", n)
	}

	// No seal was recorded, so the same identity is admitted once decisions return.
	allowed := api.NewRecoverableAcceptanceClient(frames{p, "alice", granting(accessAllowed)})
	first := accept(t, allowed, s)

	// A later outage returns unavailable for the accepted identity too, and an
	// evaluated refusal stays forbidden.
	if v, err := outage.Submit(s); err != nil || v.Outcome != "unavailable" || v.Receipt != nil {
		t.Fatalf("duplicate during outage: %+v %v", v, err)
	}
	denied := api.NewRecoverableAcceptanceClient(frames{p, "alice", granting(accessDenied)})
	if v, err := denied.Submit(s); err != nil || v.Outcome != "forbidden" {
		t.Fatalf("denied submit: %+v %v", v, err)
	}
	if again := accept(t, allowed, s); again.OperationId != first.OperationId {
		t.Fatalf("outage changed the operation: %s then %s", first.OperationId, again.OperationId)
	}
}
