package acceptanceprovider

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"

	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

const testDependentPromise = "example.execution/dependent@1"

// checkingExecutor refuses admission while its answer says so, and offers its
// dependent promise only while the dependency answers.
type checkingExecutor struct {
	testExecutor
	mu        *sync.Mutex
	answer    *api.AcceptanceOutcome
	reason    string
	asked     *int
	available *bool
}

func (e checkingExecutor) CheckAdmission(scope, kind string, spec []byte, required []string) (api.AcceptanceOutcome, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	*e.asked++
	return *e.answer, e.reason
}

func (e checkingExecutor) ExecutionGuarantees() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if *e.available {
		return []string{testDependentPromise}
	}
	return nil
}

func (e checkingExecutor) RecoveryGuarantees() []string { return []string{testDependentPromise} }

func (e checkingExecutor) PrepareWithGuarantees(id, kind string, spec []byte, required []string) ([]byte, []string, error) {
	work, err := e.Prepare(id, kind, spec)
	return work, slices.Clone(required), err
}

func newCheckingExecutor() checkingExecutor {
	answer, available, asked := api.AcceptanceOutcome(0), true, 0
	return checkingExecutor{testExecutor: testExecutor{profile: "checking-v1", prefix: "private/"}, mu: &sync.Mutex{}, answer: &answer, reason: "credential:unknown:hf", asked: &asked, available: &available}
}

func (e checkingExecutor) set(answer api.AcceptanceOutcome, available bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	*e.answer, *e.available = answer, available
}

// TestAdmissionCheckRefusesWithoutJournalOrSeal proves JOB-A16: an executor's
// refusal is invalid or unavailable with its reason, leaves no journal, seal or
// work, and the same identity is admitted once the executor admits it. A
// duplicate of accepted work is answered from its journal without asking.
func TestAdmissionCheckRefusesWithoutJournalOrSeal(t *testing.T) {
	root := t.TempDir()
	e := newCheckingExecutor()
	p, err := OpenWithExecutor(root, "owner", e)
	if err != nil {
		t.Fatal(err)
	}
	s := submission(p, "named-credential")
	for _, word := range []api.AcceptanceOutcome{api.AcceptanceOutcomeInvalid, api.AcceptanceOutcomeUnavailable} {
		e.set(word, true)
		r, err := p.Bind("alice").Submit(s)
		if err != nil || r.Outcome != word || r.Reason != "credential:unknown:hf" || r.Receipt != nil {
			t.Fatalf("%s: %+v %v", word, r, err)
		}
		entries, err := os.ReadDir(filepath.Join(root, "acceptance", "requests"))
		if err != nil || len(entries) != 0 || jobs(t, root) != 0 {
			t.Fatalf("%s left evidence: %v %v", word, entries, err)
		}
	}
	// Reconcile never asks, and seals as it always did.
	sealedID := api.RequestIdentity{Key: "reconciled-first", HistoryEpoch: p.config.Epoch}
	asked := *e.asked
	if r, err := p.Bind("alice").Reconcile(sealedID); err != nil || r.Outcome.String() != "definitely_not_accepted" || *e.asked != asked {
		t.Fatalf("reconcile: %+v %v asked %d", r, err, *e.asked-asked)
	}
	e.set(0, true)
	receipt := accept(t, p.Bind("alice"), s)
	e.set(api.AcceptanceOutcomeInvalid, true)
	asked = *e.asked
	if r, err := p.Bind("alice").Submit(s); err != nil || r.Outcome.String() != "accepted" || r.Receipt == nil || !reflect.DeepEqual(*r.Receipt, receipt) || *e.asked != asked {
		t.Fatalf("duplicate of accepted work: %+v %v asked %d", r, err, *e.asked-asked)
	}
	// An unsupported guarantee seals as before; the executor is not asked.
	e.set(api.AcceptanceOutcomeInvalid, false)
	unmet := submission(p, "unmet")
	unmet.RequiredGuarantees = append(unmet.RequiredGuarantees, testDependentPromise)
	if r, err := p.Bind("alice").Submit(unmet); err != nil || r.Outcome.String() != "definitely_not_accepted" || *e.asked != asked {
		t.Fatalf("unsupported guarantee: %+v %v asked %d", r, err, *e.asked-asked)
	}
}

// TestRecoveryKeepsWorkWhoseDependencyIsAbsent proves a restarting provider
// opens its store while an accepted journal requires a guarantee the executor
// cannot offer now, keeps that work accepted, and refuses only new submissions
// requiring it until the dependency answers.
func TestRecoveryKeepsWorkWhoseDependencyIsAbsent(t *testing.T) {
	root := t.TempDir()
	e := newCheckingExecutor()
	p, err := OpenWithExecutor(root, "owner", e)
	if err != nil {
		t.Fatal(err)
	}
	constrained := submission(p, "constrained")
	constrained.RequiredGuarantees = append(constrained.RequiredGuarantees, testDependentPromise)
	receipt := accept(t, p.Bind("alice"), constrained)
	plain := accept(t, p.Bind("alice"), submission(p, "plain"))

	e.set(0, false)
	p, err = OpenWithExecutor(root, "owner", e)
	if err != nil {
		t.Fatalf("an absent dependency closed the store: %v", err)
	}
	if slices.Contains(p.SupportedGuarantees(), testDependentPromise) {
		t.Fatal("offered a guarantee whose dependency is absent")
	}
	if r, err := p.Bind("alice").Submit(constrained); err != nil || r.Outcome.String() != "accepted" || !reflect.DeepEqual(*r.Receipt, receipt) {
		t.Fatalf("accepted work after restart: %+v %v", r, err)
	}
	if r, err := p.Bind("alice").Reconcile(plain.Identity); err != nil || r.Outcome.String() != "accepted" || !reflect.DeepEqual(*r.Receipt, plain) {
		t.Fatalf("other work after restart: %+v %v", r, err)
	}
	fresh := constrained
	fresh.Identity.Key = "fresh-while-absent"
	if r, err := p.Bind("alice").Submit(fresh); err != nil || r.Outcome.String() != "definitely_not_accepted" {
		t.Fatalf("new work requiring the absent dependency: %+v %v", r, err)
	}
	e.set(0, true)
	later := constrained
	later.Identity.Key = "fresh-once-available"
	accept(t, p.Bind("alice"), later)

	// A profile that does not recover the guarantee still refuses the store.
	if _, err := OpenWithExecutor(root, "owner", promisedExecutor{testExecutor: testExecutor{profile: "checking-v1", prefix: "private/"}}); err == nil {
		t.Fatal("a profile without the guarantee opened work requiring it")
	}
}
