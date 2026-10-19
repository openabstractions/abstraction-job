package acceptanceprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	cas "github.com/openabstractions/abstraction-cas/go"
	job "github.com/openabstractions/abstraction-job/go"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

type canonicalBlockedChecker struct {
	testExecutor
	entered, release chan struct{}
	once             *sync.Once
}

func (e canonicalBlockedChecker) CheckAdmission(scope, kind string, spec []byte, required []string) (api.AcceptanceOutcome, string) {
	if strings.HasPrefix(scope, nativeOwnershipProfile+":") {
		e.once.Do(func() { close(e.entered) })
		<-e.release
		return api.AcceptanceOutcomeForbidden, "controlled canonical refusal"
	}
	return api.AcceptanceOutcomeAccepted, ""
}

func TestCanonicalNativeRefusalCallbackYieldsToExactLegacyReservation(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-intent", true: "changed-intent"}[changed], func(t *testing.T) {
			root := t.TempDir()
			e := canonicalBlockedChecker{testExecutor: testExecutor{profile: "blocked-check-v1", prefix: "private/"}, entered: make(chan struct{}), release: make(chan struct{}), once: new(sync.Once)}
			p, err := OpenWithExecutor(root, "logical-owner", e)
			if err != nil {
				t.Fatal(err)
			}
			a, _ := canonicalVersions(t)
			enableCanonical(t, p)
			s := submission(p, "blocked-canonical-refusal")
			done := make(chan api.AcceptanceResult, 1)
			errorsSeen := make(chan error, 1)
			var release sync.Once
			unblock := func() { release.Do(func() { close(e.release) }) }
			t.Cleanup(unblock)
			go func() { r, err := p.BindBinding(a).Submit(s); errorsSeen <- err; done <- r }()
			select {
			case <-e.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("canonical checker did not block")
			}
			legacy := s
			if changed {
				legacy.Spec = []byte(`{"source":"different intent"}`)
			}
			accepted := accept(t, p.Bind(a.Scope), legacy)
			unblock()
			var got api.AcceptanceResult
			select {
			case got = <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("canonical call did not join")
			}
			if err := <-errorsSeen; err != nil {
				t.Fatal(err)
			}
			if changed {
				if got.Outcome != api.AcceptanceOutcomeKeyConflict {
					t.Fatal("callback masked gained conflicting journal", got)
				}
			} else {
				if got.Outcome != api.AcceptanceOutcomeAccepted || got.Receipt.OperationID != accepted.OperationID {
					t.Fatal("callback masked gained journal", got)
				}
			}
			if jobs(t, root) != 1 {
				t.Fatal("callback race reserved another effect")
			}
		})
	}
}

func TestCanonicalNativeInventoryCursorSeparatesProgramAndExactFallback(t *testing.T) {
	p := openTest(t, t.TempDir())
	defer p.CloseInventory()
	a, version := canonicalVersions(t)
	enableCanonical(t, p)
	other := a
	subject := *a.Subject
	subject.Program = "msix:OtherFixture_pzs8sxrjxfjjc"
	other.Subject = &subject
	for _, key := range []string{"a-one", "a-two", "a-three"} {
		accept(t, p.BindBinding(a), submission(p, key))
	}
	accept(t, p.BindBinding(other), submission(p, "other-program"))
	for _, account := range []bool{false, true} {
		own := p.bindInventoryBinding(a).(*bound)
		foreign := p.bindInventoryBinding(other).(*bound)
		first, err := own.listWork("", 1, account)
		if err != nil || first.Outcome != api.InventoryOutcomePage || first.Next == "" {
			t.Fatal(first, err)
		}
		cursor := first.Next
		if got, _ := foreign.listWork(cursor, 1, account); got.Outcome != api.InventoryOutcomeGap {
			t.Fatal("another Program continued native inventory", got)
		}
		page, err := own.listWork(cursor, 1, account)
		if err != nil || page.Outcome != api.InventoryOutcomePage {
			t.Fatal(page, err)
		}
		if got, _ := foreign.listWork(cursor, 1, account); got.Outcome != api.InventoryOutcomeGap {
			t.Fatal("another Program replayed cached native page", got)
		}
		if got, _ := p.bindInventoryBinding(version).(*bound).listWork(cursor, 1, account); got.Outcome != api.InventoryOutcomeGap {
			t.Fatal("another exact executable replayed fallback page", got)
		}
		if got, _ := p.BindInventory(a.Scope).(*bound).listWork(cursor, 1, account); got.Outcome != api.InventoryOutcomeGap {
			t.Fatal("unbound path profile replayed native cursor", got)
		}
		again, err := own.listWork(cursor, 1, account)
		if err != nil || again.Outcome != api.InventoryOutcomePage {
			t.Fatal("own cached replay lost", again, err)
		}
	}
}

type canonicalLockCheckingExecutor struct {
	testExecutor
	root string
}

func (e canonicalLockCheckingExecutor) Prepare(id, kind string, spec []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	unlock, err := cas.LockContext(ctx, filepath.Join(e.root, "acceptance", "admission"), false)
	if err != nil {
		return nil, err
	}
	unlock()
	return e.testExecutor.Prepare(id, kind, spec)
}

func TestCanonicalNativeActivationRefreshesAlreadyOpenRecoveryIndex(t *testing.T) {
	root := t.TempDir()
	old := openTest(t, root)
	next := openTest(t, root)
	a, b := canonicalVersions(t)
	s := submission(old, "late-before-activation")
	first := accept(t, old.BindBinding(a), s)
	enableCanonical(t, next)
	if got := accept(t, next.BindBinding(b), s); got.OperationID != first.OperationID || jobs(t, root) != 1 {
		t.Fatal(got)
	}
}

func TestCanonicalNativePreparationAndRecoveryOutsideRootLock(t *testing.T) {
	root := t.TempDir()
	e := canonicalLockCheckingExecutor{testExecutor: testExecutor{profile: "test-v1", prefix: "private/"}, root: root}
	p, err := OpenWithExecutor(root, "logical-owner", e)
	if err != nil {
		t.Fatal(err)
	}
	a, b := canonicalVersions(t)
	enableCanonical(t, p)
	s := submission(p, "outside-root-lock")
	first := accept(t, p.BindBinding(a), s)
	if got := accept(t, p.BindBinding(b), s); got.OperationID != first.OperationID {
		t.Fatal(got)
	}
	p, err = OpenWithExecutor(root, "logical-owner", e)
	if err != nil {
		t.Fatal(err)
	}
	enableCanonical(t, p)
	if got := accept(t, p.BindBinding(b), s); got.OperationID != first.OperationID {
		t.Fatal(got)
	}
}

func canonicalVersions(t *testing.T) (Binding, Binding) {
	t.Helper()
	a := testBinding(t)
	a.Subject.Program = "msix:SubjectFixture_pzs8sxrjxfjjc"
	subject := *a.Subject
	subject.Executable = filepath.Join(filepath.Dir(subject.Executable), "version2", "caller.exe")
	scope, err := LocalSubjectScope(subject.AccountKind, subject.Account, subject.Executable)
	if err != nil {
		t.Fatal(err)
	}
	return a, Binding{Scope: scope, Subject: &subject, Origin: "local"}
}

func enableCanonical(t *testing.T, p *Provider) {
	t.Helper()
	if err := p.EnableCanonicalSubjects(); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalNativeLegacyReplayRestartAndControls(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	a, b := canonicalVersions(t)
	s := submission(p, "same-native")
	first := accept(t, p.BindBinding(a), s)
	path := p.requestPath(a.Scope, s.Identity)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireHost(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	enableCanonical(t, p)
	replay := accept(t, p.BindBinding(b), s)
	if replay.OperationID != first.OperationID || jobs(t, root) != 1 {
		t.Fatal(replay)
	}
	changed := s
	changed.Spec = []byte(`{"different":true}`)
	if got := outcomeOf(t)(p.BindBinding(b).Submit(changed)); got != "key_conflict" {
		t.Fatal(got)
	}
	if got, err := p.BindOperationsBinding(b).ObserveWork(s.Identity); err != nil || got.Outcome != api.ObservationOutcomeObserved || got.Snapshot.Receipt.OperationID != first.OperationID {
		t.Fatal(got, err)
	}
	page, err := p.bindInventoryBinding(b).ListWork("", 64)
	if err != nil || len(page.Snapshots) != 1 || page.Snapshots[0].Receipt.OperationID != first.OperationID {
		t.Fatal(page, err)
	}
	if got, err := p.BindBinding(b).CancelWork(s.Identity); err != nil || got.Outcome != api.CancellationOutcomeRequested {
		t.Fatal(got, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("old journal was rewritten", err)
	}
	p = openTest(t, root)
	if got := outcomeOf(t)(p.BindBinding(b).Submit(s)); got != "unknown" {
		t.Fatal("unconfigured restart admitted", got)
	}
	enableCanonical(t, p)
	if got := accept(t, p.BindBinding(b), s); got.OperationID != first.OperationID {
		t.Fatal(got)
	}
	if _, found, err := p.SubjectForOperation(first.OperationID, a.Scope); err != nil || !found {
		t.Fatal(found, err)
	}
}

func TestCanonicalNativeNewJournalAndAttemptAcrossVersions(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	a, b := canonicalVersions(t)
	enableCanonical(t, p)
	s := submission(p, "new-native")
	first := accept(t, p.BindBinding(a), s)
	scope, err := canonicalSubjectScope(a)
	if err != nil {
		t.Fatal(err)
	}
	j, _, err := p.readJournal(scope, s.Identity)
	if err != nil || j == nil || j.OwnershipProfile != nativeOwnershipProfile || j.Subject.Executable != a.Subject.Executable {
		t.Fatal(j, err)
	}
	if replay := accept(t, p.BindBinding(b), s); replay.OperationID != first.OperationID {
		t.Fatal(replay)
	}
	endOperation(t, p, first.OperationID, job.StateFailed)
	next := accept(t, p.BindBinding(b), retry(s, 1))
	if next.OperationID == first.OperationID || jobs(t, root) != 2 {
		t.Fatal(next)
	}
	p = openTest(t, root)
	enableCanonical(t, p)
	if replay := accept(t, p.BindBinding(a), retry(s, 1)); replay.OperationID != next.OperationID {
		t.Fatal(replay)
	}
}

func TestCanonicalNativeDistinctLegacyOccurrencesConflict(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	a, b := canonicalVersions(t)
	s := submission(p, "ambiguous")
	accept(t, p.BindBinding(a), s)
	accept(t, p.BindBinding(b), s)
	enableCanonical(t, p)
	for _, binding := range []Binding{a, b} {
		if got := outcomeOf(t)(p.BindBinding(binding).Submit(s)); got != "key_conflict" {
			t.Fatal(got)
		}
	}
	if jobs(t, root) != 2 {
		t.Fatal("created another effect reservation")
	}
}

func TestCanonicalNativeConcurrentReservationAndFreshDefaultFence(t *testing.T) {
	root := t.TempDir()
	old := openTest(t, root)
	p := openTest(t, root)
	a, b := canonicalVersions(t)
	s := submission(p, "concurrent")
	lease, err := AcquireHost(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	enableCanonical(t, p)
	if got := outcomeOf(t)(old.BindBinding(a).Submit(s)); got != "unknown" {
		t.Fatal("already-open0.8 default bypassed marker", got)
	}
	var wg sync.WaitGroup
	results := make(chan api.AcceptanceResult, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			binding := a
			if i%2 != 0 {
				binding = b
			}
			r, _ := p.BindBinding(binding).Submit(s)
			results <- r
		}()
	}
	wg.Wait()
	close(results)
	id := ""
	for r := range results {
		if r.Outcome != api.AcceptanceOutcomeAccepted {
			t.Fatal(r)
		}
		if id != "" && id != r.Receipt.OperationID {
			t.Fatal("duplicate receipts")
		}
		id = r.Receipt.OperationID
	}
	if jobs(t, root) != 1 {
		t.Fatal("duplicate reservation")
	}
	if other, err := AcquireHost(root); err == nil {
		other.Close()
		t.Fatal("host fence lost")
	}
}

func TestCanonicalNativeSeparateConfiguredReadersRefreshFeatureHeader(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	other := openTest(t, root)
	a, b := canonicalVersions(t)
	enableCanonical(t, p)
	enableCanonical(t, other)
	s := submission(p, "separate-readers")
	first := accept(t, p.BindBinding(a), s)
	if got := accept(t, other.BindBinding(b), s); got.OperationID != first.OperationID || jobs(t, root) != 1 {
		t.Fatal(got)
	}
}

func TestCanonicalNativeUnavailableProvenanceIsNotEmpty(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	a, b := canonicalVersions(t)
	s := submission(p, "corrupt")
	accept(t, p.BindBinding(a), s)
	enableCanonical(t, p)
	path := p.requestPath(a.Scope, s.Identity)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["SubjectOrigin"] = "remote"
	raw, _ = json.Marshal(doc)
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(t)(p.BindBinding(b).Submit(s)); got != "unknown" {
		t.Fatal(got)
	}
	if jobs(t, root) != 1 {
		t.Fatal("unavailable provenance became new operation")
	}
}

func TestCanonicalNativeMissingIndexedJournalIsNotFreshNamespace(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	a, b := canonicalVersions(t)
	s := submission(p, "missing-indexed")
	accept(t, p.BindBinding(a), s)
	enableCanonical(t, p)
	if err := os.Remove(p.requestPath(a.Scope, s.Identity)); err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(t)(p.BindBinding(b).Submit(s)); got != "unknown" {
		t.Fatal(got)
	}
	if jobs(t, root) != 1 {
		t.Fatal("missing retained provenance became second operation")
	}
}

func TestCanonicalNativeExactCurrentSubjectlessLegacyKeepsOriginalReceipt(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	a, _ := canonicalVersions(t)
	s := submission(p, "subjectless-legacy")
	first := accept(t, p.Bind(a.Scope), s)
	enableCanonical(t, p)
	if got := accept(t, p.BindBinding(a), s); got.OperationID != first.OperationID || jobs(t, root) != 1 {
		t.Fatal(got)
	}
	j, _, err := p.readJournal(a.Scope, s.Identity)
	if err != nil || j.Subject != nil || j.OwnershipProfile != "" {
		t.Fatal("legacy history relabelled", j, err)
	}
	if owned, err := p.OperationOwnedByBinding(first.OperationID, a); err != nil || !owned {
		t.Fatal(owned, err)
	}
	if err = os.Remove(p.requestPath(a.Scope, s.Identity)); err != nil {
		t.Fatal(err)
	}
	if owned, err := p.OperationOwnedByBinding(first.OperationID, a); err == nil || owned {
		t.Fatal("missing subjectless history granted from cache", owned, err)
	}
}

func TestCanonicalNativeNegativeSealSurvivesVersionAndRestart(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	a, b := canonicalVersions(t)
	enableCanonical(t, p)
	s := submission(p, "sealed-native")
	if got := outcomeOf(t)(p.BindBinding(a).Reconcile(s.Identity)); got != "definitely_not_accepted" {
		t.Fatal(got)
	}
	p = openTest(t, root)
	enableCanonical(t, p)
	if got := outcomeOf(t)(p.BindBinding(b).Submit(s)); got != "definitely_not_accepted" {
		t.Fatal(got)
	}
	if jobs(t, root) != 0 {
		t.Fatal("delayed submission escaped canonical seal")
	}
}

func TestCanonicalNativeAttemptRetainsExactCurrentSubjectlessPredecessor(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	a, _ := canonicalVersions(t)
	s := submission(p, "subjectless-attempt")
	first := accept(t, p.Bind(a.Scope), s)
	endOperation(t, p, first.OperationID, job.StateFailed)
	enableCanonical(t, p)
	next := accept(t, p.BindBinding(a), retry(s, 1))
	if next.OperationID == first.OperationID || jobs(t, root) != 2 {
		t.Fatal(next)
	}
}

func TestCanonicalNativeActivationSnapshotIncludesExactHeader(t *testing.T) {
	p := openTest(t, t.TempDir())
	snapshot, err := p.activationSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err = p.ensureFeature(StorageFeatureLabels); err != nil {
		t.Fatal(err)
	}
	unlock, err := p.lockAdmission()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err = snapshot.unchanged(p); !errors.Is(err, errOwnershipChanged) {
		t.Fatal("activation accepted changed owner header", err)
	}
}

func TestCanonicalNativeFeatureLossOrUnsupportedActivationRefuses(t *testing.T) {
	for _, loss := range []bool{true, false} {
		t.Run(map[bool]string{true: "lost-marker", false: "unsupported-header"}[loss], func(t *testing.T) {
			root := t.TempDir()
			p := openTest(t, root)
			a, _ := canonicalVersions(t)
			enableCanonical(t, p)
			path := filepath.Join(root, "acceptance", "owner.json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var c configuration
			if err = json.Unmarshal(raw, &c); err != nil {
				t.Fatal(err)
			}
			if loss {
				c.Features = nil
			} else {
				c.Features = append(c.Features, "unsupported-future-profile")
			}
			raw, _ = json.Marshal(c)
			if err = os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if got := outcomeOf(t)(p.BindBinding(a).Submit(submission(p, "refuse-header"))); got != "unknown" {
				t.Fatal(got)
			}
			if jobs(t, root) != 0 {
				t.Fatal("header failure admitted work")
			}
		})
	}
}
