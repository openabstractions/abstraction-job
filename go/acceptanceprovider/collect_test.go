package acceptanceprovider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	cas "github.com/openabstractions/abstraction-cas/go"
	"github.com/openabstractions/abstraction-identity/fileid"
	job "github.com/openabstractions/abstraction-job/go"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

type collectingExecutor struct{ linkExecutor }

type collectionReaderFixture struct {
	collectingExecutor
	entered chan struct{}
	release chan struct{}
}

func (e collectionReaderFixture) ReadOperationResult(root string, r *job.Record, offset, maximum int64) ([]byte, int64, error) {
	close(e.entered)
	<-e.release
	return e.collectingExecutor.ReadOperationResult(root, r, offset, maximum)
}

func (collectingExecutor) ResultRetentionMs() int64 { return MinimumRetentionMs }
func (collectingExecutor) OperationTmpNames(r *job.Record) (string, string, error) {
	return "results/" + r.ID, "work/" + r.ID, nil
}
func (e collectingExecutor) PrepareLegacy(id, kind string, spec []byte, _ []string) ([]byte, []string, error) {
	work, err := e.Prepare(id, kind, spec)
	return work, nil, err
}

var collectionTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func collectionFixture(t *testing.T, state job.State) *linkFixture {
	t.Helper()
	f := newLinkFixture(t, true)
	f.p.executor = collectingExecutor{f.p.executor.(linkExecutor)}
	editCollectionRecord(t, f, func(r *job.Record) {
		r.State, r.UpdatedAt = state, job.At(collectionTime)
	})
	if err := os.WriteFile(f.p.store.WorkPath(f.receipt.OperationID), []byte("partial fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func editCollectionRecord(t *testing.T, f *linkFixture, edit func(*job.Record)) {
	t.Helper()
	path := filepath.Join(f.p.root, "jobs", f.receipt.OperationID+".json")
	if err := cas.Change(path, func(raw []byte) ([]byte, error) {
		r, err := job.Decode(raw)
		if err != nil {
			return nil, err
		}
		edit(r)
		return r.Encode()
	}); err != nil {
		t.Fatal(err)
	}
}

func collectFixture(t *testing.T, f *linkFixture, age, grace time.Duration) []TmpRemoval {
	t.Helper()
	removed, err := f.p.CollectTmp(context.Background(), collectionTime.Add(age), grace)
	if err != nil {
		t.Fatal(err)
	}
	return removed
}

func requireKept(t *testing.T, name string) {
	t.Helper()
	if _, err := os.Lstat(name); err != nil {
		t.Fatal("expected retained name", err)
	}
}

func TestCollectTmpResultWaitsSumAndRecordsLoss(t *testing.T) {
	f := collectionFixture(t, job.StateComplete)
	grace := 10 * 24 * time.Hour
	for _, age := range []time.Duration{grace, 24*time.Hour + grace - time.Millisecond} {
		if removed := collectFixture(t, f, age, grace); len(removed) != 0 {
			t.Fatal(removed)
		}
		requireKept(t, f.result)
	}
	if removed := collectFixture(t, f, 24*time.Hour+grace, grace); len(removed) != 1 || removed[0].Name != "results/"+f.receipt.OperationID {
		t.Fatal(removed)
	}
	if !f.p.resultLost(f.local.Scope, f.id) {
		t.Fatal("collection did not record result loss")
	}
	if removed := collectFixture(t, f, 24*time.Hour+grace, grace); len(removed) != 0 {
		t.Fatal("repeated removal", removed)
	}
	// Complete work is not an abandoned partial.
	requireKept(t, f.p.store.WorkPath(f.receipt.OperationID))
}

func TestCollectTmpLegacyOriginPreservesCanonicalLookingResult(t *testing.T) {
	f := collectionFixture(t, job.StateComplete)
	path := f.p.requestPath(f.local.Scope, f.id)
	var retained journal
	if err := cas.Change(path, func(raw []byte) ([]byte, error) {
		if err := json.Unmarshal(raw, &retained); err != nil {
			return nil, err
		}
		retained.Origin = legacyMigrationOrigin
		retained.Subject, retained.SubjectOrigin, retained.SubjectEvidence = nil, "", ""
		return json.Marshal(retained)
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.p.indexJournal(path, &retained); err != nil {
		t.Fatal(err)
	}
	if removed := collectFixture(t, f, 100*24*time.Hour, time.Hour); len(removed) != 0 {
		t.Fatal(removed)
	}
	requireKept(t, f.result)
}

func TestCollectTmpLinkResultAliasHoldsUntilRemoved(t *testing.T) {
	f := collectionFixture(t, job.StateComplete)
	alias := filepath.Join(t.TempDir(), "provider-model")
	if linked := f.link(t, f.local, alias); linked.Outcome != api.LinkOutcomeLinked {
		t.Fatal(linked)
	}
	if removed, err := f.p.CollectTmp(context.Background(), collectionTime.Add(100*24*time.Hour), time.Hour); err == nil || len(removed) != 0 {
		t.Fatal("aliased result was collected", removed, err)
	}
	requireKept(t, f.result)
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if removed := collectFixture(t, f, 100*24*time.Hour, time.Hour); len(removed) != 1 {
		t.Fatal(removed)
	}
}

func TestCollectTmpUnfinishedPartialSurvivesAnyLeaseAge(t *testing.T) {
	for _, state := range []job.State{job.StatePending, job.StateRunning, job.StateTransferred} {
		for _, pause := range []bool{false, true} {
			t.Run(string(state)+map[bool]string{true: "-paused"}[pause], func(t *testing.T) {
				f := collectionFixture(t, state)
				editCollectionRecord(t, f, func(r *job.Record) {
					r.Lease.ExpiresAt = job.At(collectionTime.Add(-time.Hour))
					if pause {
						r.Intent = &job.Intent{Want: job.WantPause}
					}
				})
				if removed := collectFixture(t, f, 100*24*time.Hour, 24*time.Hour); len(removed) != 0 {
					t.Fatal(removed)
				}
				requireKept(t, f.p.store.WorkPath(f.receipt.OperationID))
			})
		}
	}
}

func TestCollectTmpFailedAndCancelledPartialGrace(t *testing.T) {
	for _, state := range []job.State{job.StateFailed, job.StateCancelled} {
		t.Run(string(state), func(t *testing.T) {
			f := collectionFixture(t, state)
			if removed := collectFixture(t, f, 24*time.Hour-time.Millisecond, 24*time.Hour); len(removed) != 0 {
				t.Fatal(removed)
			}
			requireKept(t, f.p.store.WorkPath(f.receipt.OperationID))
			if removed := collectFixture(t, f, 24*time.Hour, 24*time.Hour); len(removed) != 1 {
				t.Fatal(removed)
			}
			requireKept(t, f.result)
		})
	}
}

func TestCollectTmpKeepsMissingOrContradictoryEvidenceAndStrays(t *testing.T) {
	for _, mode := range []string{"missing-journal", "unreadable-journal", "record-mismatch", "future-time", "missing-time"} {
		t.Run(mode, func(t *testing.T) {
			f := collectionFixture(t, job.StateComplete)
			path := f.p.requestPath(f.local.Scope, f.id)
			switch mode {
			case "missing-journal":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "unreadable-journal":
				if err := os.WriteFile(path, []byte("not JSON"), 0600); err != nil {
					t.Fatal(err)
				}
			case "record-mismatch":
				editCollectionRecord(t, f, func(r *job.Record) { r.Spec = []byte(`{"different":true}`) })
			case "future-time":
				editCollectionRecord(t, f, func(r *job.Record) { r.UpdatedAt = job.At(collectionTime.Add(200 * 24 * time.Hour)) })
			case "missing-time":
				editCollectionRecord(t, f, func(r *job.Record) { r.UpdatedAt = job.Timestamp{} })
			}
			stray := filepath.Join(f.p.root, "results", "stray")
			if err := os.WriteFile(stray, []byte("unindexed fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			if removed, err := f.p.CollectTmp(context.Background(), collectionTime.Add(100*24*time.Hour), time.Hour); err == nil || len(removed) != 0 {
				t.Fatal(removed, err)
			}
			requireKept(t, f.result)
			requireKept(t, stray)
		})
	}
}

func TestCollectTmpKeepsDirectoriesAndLinkedNames(t *testing.T) {
	for _, mode := range []string{"directory", "leaf-symlink", "parent-symlink"} {
		t.Run(mode, func(t *testing.T) {
			f := collectionFixture(t, job.StateComplete)
			outside := filepath.Join(t.TempDir(), f.receipt.OperationID)
			if err := os.WriteFile(outside, []byte("caller fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			if mode == "parent-symlink" {
				parent := filepath.Dir(f.result)
				if err := os.Rename(parent, parent+"-kept"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Dir(outside), parent); err != nil {
					t.Skip("symlink fixture unavailable", err)
				}
			} else {
				if err := os.Remove(f.result); err != nil {
					t.Fatal(err)
				}
				if mode == "directory" {
					if err := os.Mkdir(f.result, 0700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink(outside, f.result); err != nil {
					t.Skip("symlink fixture unavailable", err)
				}
			}
			if removed, err := f.p.CollectTmp(context.Background(), collectionTime.Add(100*24*time.Hour), time.Hour); err == nil || len(removed) != 0 {
				t.Fatal(removed, err)
			}
			requireKept(t, f.result)
			requireKept(t, outside)
		})
	}
}

func TestCollectTmpUnknownLinkCountKeepsResult(t *testing.T) {
	f := collectionFixture(t, job.StateComplete)
	prior := tmpLinkCount
	tmpLinkCount = func(fileid.FileID) int { return 0 }
	defer func() { tmpLinkCount = prior }()
	if removed, err := f.p.CollectTmp(context.Background(), collectionTime.Add(100*24*time.Hour), time.Hour); err == nil || len(removed) != 0 {
		t.Fatal(removed, err)
	}
	requireKept(t, f.result)
}

func TestCollectTmpOpenHandleAndInterruptedLossRecording(t *testing.T) {
	f := collectionFixture(t, job.StateComplete)
	if runtime.GOOS == "windows" {
		held, err := os.Open(f.result)
		if err != nil {
			t.Fatal(err)
		}
		removed, err := f.p.CollectTmp(context.Background(), collectionTime.Add(100*24*time.Hour), time.Hour)
		held.Close()
		if err == nil || len(removed) != 0 {
			t.Fatal("open handle was not retained", removed, err)
		}
		requireKept(t, f.result)
	}
	// An interruption after unlink and before ResultLost is recovered from
	// absence, using the still-retained journal rather than a new ledger.
	if err := os.Remove(f.result); err != nil {
		t.Fatal(err)
	}
	if removed := collectFixture(t, f, 100*24*time.Hour, time.Hour); len(removed) != 0 {
		t.Fatal(removed)
	}
	if !f.p.resultLost(f.local.Scope, f.id) {
		t.Fatal("missing result loss not recorded")
	}
	if removed := collectFixture(t, f, 100*24*time.Hour, time.Hour); len(removed) != 0 {
		t.Fatal(removed)
	}
}

func TestCollectTmpRejectsInvalidPolicyAndAbsentNamer(t *testing.T) {
	f := collectionFixture(t, job.StateComplete)
	if _, err := f.p.CollectTmp(context.Background(), collectionTime, 0); err == nil {
		t.Fatal("zero accepted")
	}
	f.p.executor = f.p.executor.(collectingExecutor).linkExecutor
	if removed, err := f.p.CollectTmp(context.Background(), collectionTime.Add(100*24*time.Hour), time.Hour); err != nil || len(removed) != 0 {
		t.Fatal(removed, err)
	}
	requireKept(t, f.result)
	if _, err := f.p.tmpCandidate("unknown", collectionTime, time.Hour, false); err == nil {
		t.Fatal("missing journal silently admitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.p.executor = collectingExecutor{f.p.executor.(linkExecutor)}
	if _, err := f.p.CollectTmp(ctx, collectionTime, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCollectTmpWaitsForReadResult(t *testing.T) {
	f := collectionFixture(t, job.StateComplete)
	e := collectionReaderFixture{collectingExecutor: f.p.executor.(collectingExecutor), entered: make(chan struct{}), release: make(chan struct{})}
	f.p.executor = e
	read := make(chan api.ResultRead, 1)
	go func() {
		answer, _ := f.p.BindOperations(f.local.Scope).ReadResult(f.id, 0, MaxResultBytes)
		read <- answer
	}()
	<-e.entered
	collected := make(chan error, 1)
	go func() {
		_, err := f.p.CollectTmp(context.Background(), collectionTime.Add(100*24*time.Hour), time.Hour)
		collected <- err
	}()
	select {
	case err := <-collected:
		close(e.release)
		<-read
		t.Fatal("collection passed an active result read", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(e.release)
	if answer := <-read; answer.Outcome != api.ResultOutcomeData {
		t.Fatal(answer)
	}
	if err := <-collected; err != nil {
		t.Fatal(err)
	}
}

func TestCollectTmpWaitsForLinkResultAndRechecksAlias(t *testing.T) {
	f := collectionFixture(t, job.StateComplete)
	entered, release := make(chan struct{}), make(chan struct{})
	original := linkFile
	linkFile = func(source string, parent *heldLinkParent, leaf string) error {
		close(entered)
		<-release
		return original(source, parent, leaf)
	}
	defer func() { linkFile = original }()
	destination := filepath.Join(t.TempDir(), "provider-model")
	linked := make(chan api.ResultLink, 1)
	go func() {
		answer, _ := f.p.BindOperationsBinding(f.local).LinkResult(f.id, destination)
		linked <- answer
	}()
	<-entered
	collected := make(chan error, 1)
	go func() {
		_, err := f.p.CollectTmp(context.Background(), collectionTime.Add(100*24*time.Hour), time.Hour)
		collected <- err
	}()
	select {
	case err := <-collected:
		close(release)
		<-linked
		t.Fatal("collection passed an active link", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if answer := <-linked; answer.Outcome != api.LinkOutcomeLinked {
		t.Fatal(answer)
	}
	if err := <-collected; err == nil {
		t.Fatal("collection did not recheck the new alias")
	}
	requireKept(t, f.result)
}
