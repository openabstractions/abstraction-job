package acceptanceprovider

import (
	"slices"
	"testing"

	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

// undecided dispatches like a host with no configured method policy.
type undecided struct {
	p     *Provider
	scope string
}

func (u undecided) ExchangeFrame(frame []byte) ([]byte, error) {
	return u.p.dispatchFrame(frame, u.scope, granting(accessAllowed), false)
}

// The operator profile lists every scope's work and cancels another scope's
// operation only when its decision permits; a denial is forbidden, an outage is
// unavailable, and a host with no decision forbids every call. Own-scope listing
// is unchanged, and an account cursor never continues an own-scope listing.
func TestOperatorListsAndCancelsAcrossScopesByDecision(t *testing.T) {
	p := openTest(t, t.TempDir())
	alice := accept(t, api.NewRecoverableAcceptanceClient(frames{p, "alice", granting(accessAllowed)}), submission(p, "alice-work"))
	bob := accept(t, api.NewRecoverableAcceptanceClient(frames{p, "bob", granting(accessAllowed)}), submission(p, "bob-work"))

	operator := api.NewJobOperatorClient(frames{p, "carol", granting(accessAllowed)})
	page, err := operator.ListAccountWork("", 1)
	if err != nil || page.Outcome.String() != "page" || len(page.Snapshots) != 1 || page.Complete || page.Next == "" {
		t.Fatalf("first account page %+v %v", page, err)
	}
	ids := []string{page.Snapshots[0].Receipt.OperationID}
	if own, err := api.NewJobInventoryClient(frames{p, "carol", granting(accessAllowed)}).ListWork(page.Next, 1); err != nil || own.Outcome.String() != "gap" {
		t.Fatalf("an account cursor continued an own-scope listing %+v %v", own, err)
	}
	for next := page.Next; next != ""; {
		more, err := operator.ListAccountWork(next, 1)
		if err != nil || more.Outcome.String() != "page" {
			t.Fatalf("account page %+v %v", more, err)
		}
		for _, s := range more.Snapshots {
			ids = append(ids, s.Receipt.OperationID)
		}
		next = more.Next
	}
	slices.Sort(ids)
	want := []string{alice.OperationID, bob.OperationID}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Fatalf("account listing %v, want %v", ids, want)
	}
	if own, err := api.NewJobInventoryClient(frames{p, "carol", granting(accessAllowed)}).ListWork("", 8); err != nil || own.Outcome.String() != "page" || len(own.Snapshots) != 0 {
		t.Fatalf("own-scope listing widened %+v %v", own, err)
	}

	observeBob := func() bool {
		t.Helper()
		v, err := api.NewOperationControlClient(frames{p, "bob", granting(accessAllowed)}).ObserveWork(bob.Identity)
		if err != nil || v.Snapshot == nil {
			t.Fatalf("bob observes %+v %v", v, err)
		}
		return v.Snapshot.CancellationRequested
	}
	for _, c := range []struct {
		name    string
		client  *api.JobOperatorClient
		list    api.InventoryOutcome
		outcome api.OperatorCancellationOutcome
	}{
		{"denied", api.NewJobOperatorClient(frames{p, "carol", granting(accessDenied)}), api.InventoryOutcomeForbidden, api.OperatorCancellationOutcomeForbidden},
		{"outage", api.NewJobOperatorClient(frames{p, "carol", granting(accessUnavailable)}), api.InventoryOutcomeUnavailable, api.OperatorCancellationOutcomeUnavailable},
		{"no decision", api.NewJobOperatorClient(undecided{p, "carol"}), api.InventoryOutcomeForbidden, api.OperatorCancellationOutcomeForbidden},
		{"no caller", api.NewJobOperatorClient(frames{p, "", granting(accessAllowed)}), api.InventoryOutcomeForbidden, api.OperatorCancellationOutcomeForbidden},
	} {
		if v, err := c.client.ListAccountWork("", 8); err != nil || v.Outcome != c.list || len(v.Snapshots) != 0 {
			t.Fatalf("%s listing %+v %v", c.name, v, err)
		}
		if v, err := c.client.CancelOperation(bob.OperationID); err != nil || v.Outcome != c.outcome {
			t.Fatalf("%s cancellation %+v %v", c.name, v, err)
		}
		if observeBob() {
			t.Fatalf("%s cancellation recorded intent", c.name)
		}
	}
	if v, err := operator.CancelOperation("NOT/an id"); err != nil || v.Outcome.String() != "invalid" {
		t.Fatalf("malformed id %+v %v", v, err)
	}
	if v, err := operator.CancelOperation("1-absent"); err != nil || v.Outcome.String() != "unknown" {
		t.Fatalf("absent id %+v %v", v, err)
	}
	if v, err := operator.CancelOperation(bob.OperationID); err != nil || v.Outcome.String() != "requested" {
		t.Fatalf("permitted cancellation %+v %v", v, err)
	}
	if !observeBob() {
		t.Fatal("permitted cancellation recorded no intent on bob's work")
	}
}
