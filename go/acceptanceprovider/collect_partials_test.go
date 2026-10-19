package acceptanceprovider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	cas "github.com/openabstractions/abstraction-cas/go"
	"github.com/openabstractions/abstraction-identity/fileid"
	job "github.com/openabstractions/abstraction-job/go"
)

const partialAge = 24 * time.Hour

func collectPartials(t *testing.T, f *linkFixture, age time.Duration) ([]TmpRemoval, error) {
	t.Helper()
	return f.p.CollectEndedPartials(context.Background(), collectionTime.Add(age), partialAge)
}

// The partial pass removes exactly one name: the partial of a failed or
// cancelled operation, once partialAge has passed since it ended. The result
// file stays, no loss is recorded, and a repeated pass removes nothing.
func TestCollectEndedPartialsRemovesOnlyEndedPartialAfterAge(t *testing.T) {
	for _, state := range []job.State{job.StateFailed, job.StateCancelled} {
		t.Run(string(state), func(t *testing.T) {
			f := collectionFixture(t, state)
			partial := f.p.store.WorkPath(f.receipt.OperationID)
			for _, age := range []time.Duration{0, time.Hour, partialAge - time.Millisecond} {
				removed, err := collectPartials(t, f, age)
				if err != nil || len(removed) != 0 {
					t.Fatalf("age %v: %v %v", age, removed, err)
				}
				requireKept(t, partial)
			}
			removed, err := collectPartials(t, f, partialAge)
			if err != nil || len(removed) != 1 || removed[0].OperationID != f.receipt.OperationID || removed[0].Name != "work/"+f.receipt.OperationID {
				t.Fatalf("ended partial: %v %v", removed, err)
			}
			if _, err := os.Lstat(partial); !os.IsNotExist(err) {
				t.Fatalf("partial still present: %v", err)
			}
			requireKept(t, f.result)
			if f.p.resultLost(f.local.Scope, f.id) {
				t.Fatal("partial pass recorded a result loss")
			}
			if removed, err := collectPartials(t, f, 100*partialAge); err != nil || len(removed) != 0 {
				t.Fatalf("repeated pass: %v %v", removed, err)
			}
		})
	}
}

// Running, pending and transferred work keeps its partial at any age, also
// with an expired lease or a pause intent. Completed work keeps both its
// result and its partial: results are outside the partial pass.
func TestCollectEndedPartialsKeepsUnfinishedAndCompletedWork(t *testing.T) {
	for _, state := range []job.State{job.StatePending, job.StateRunning, job.StateTransferred, job.StateComplete} {
		for _, pause := range []bool{false, true} {
			t.Run(string(state)+map[bool]string{true: "-paused"}[pause], func(t *testing.T) {
				f := collectionFixture(t, state)
				editCollectionRecord(t, f, func(r *job.Record) {
					r.Lease.ExpiresAt = job.At(collectionTime.Add(-time.Hour))
					if pause {
						r.Intent = &job.Intent{Want: job.WantPause}
					}
				})
				removed, err := collectPartials(t, f, 1000*partialAge)
				if err != nil || len(removed) != 0 {
					t.Fatalf("%v %v", removed, err)
				}
				requireKept(t, f.p.store.WorkPath(f.receipt.OperationID))
				requireKept(t, f.result)
				if f.p.resultLost(f.local.Scope, f.id) {
					t.Fatal("partial pass recorded a result loss")
				}
			})
		}
	}
}

// Migrated work's names belong to the caller's legacy layout and stay.
func TestCollectEndedPartialsKeepsMigratedWork(t *testing.T) {
	f := collectionFixture(t, job.StateFailed)
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
	if removed, err := collectPartials(t, f, 100*partialAge); err != nil || len(removed) != 0 {
		t.Fatalf("%v %v", removed, err)
	}
	requireKept(t, f.p.store.WorkPath(f.receipt.OperationID))
}

// Missing or contradictory evidence keeps the partial and reports why.
func TestCollectEndedPartialsKeepsOnMissingOrContradictoryEvidence(t *testing.T) {
	for _, mode := range []string{"missing-journal", "unreadable-journal", "record-mismatch", "future-time", "missing-time"} {
		t.Run(mode, func(t *testing.T) {
			f := collectionFixture(t, job.StateFailed)
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
				editCollectionRecord(t, f, func(r *job.Record) { r.UpdatedAt = job.At(collectionTime.Add(200 * partialAge)) })
			case "missing-time":
				editCollectionRecord(t, f, func(r *job.Record) { r.UpdatedAt = job.Timestamp{} })
			}
			stray := filepath.Join(f.p.root, "work", "stray")
			if err := os.WriteFile(stray, []byte("unindexed fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			if removed, err := collectPartials(t, f, 100*partialAge); err == nil || len(removed) != 0 {
				t.Fatalf("%v %v", removed, err)
			}
			requireKept(t, f.p.store.WorkPath(f.receipt.OperationID))
			requireKept(t, stray)
		})
	}
}

// A partial that is a directory, a link, or a file with another name keeps
// every name, including the caller's file a link points at.
func TestCollectEndedPartialsKeepsDirectoriesLinksAndAliases(t *testing.T) {
	for _, mode := range []string{"directory", "leaf-symlink", "hard-link", "unknown-link-count"} {
		t.Run(mode, func(t *testing.T) {
			f := collectionFixture(t, job.StateCancelled)
			partial := f.p.store.WorkPath(f.receipt.OperationID)
			outside := filepath.Join(t.TempDir(), "caller-file")
			if err := os.WriteFile(outside, []byte("caller fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "directory":
				if err := os.Remove(partial); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(partial, 0700); err != nil {
					t.Fatal(err)
				}
			case "leaf-symlink":
				if err := os.Remove(partial); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, partial); err != nil {
					t.Skip("symlink fixture unavailable", err)
				}
			case "hard-link":
				if err := os.Link(partial, filepath.Join(t.TempDir(), "alias")); err != nil {
					t.Skip("hard-link fixture unavailable", err)
				}
			case "unknown-link-count":
				prior := tmpLinkCount
				tmpLinkCount = func(fileid.FileID) int { return 0 }
				t.Cleanup(func() { tmpLinkCount = prior })
			}
			if removed, err := collectPartials(t, f, 100*partialAge); err == nil || len(removed) != 0 {
				t.Fatalf("%v %v", removed, err)
			}
			requireKept(t, partial)
			requireKept(t, outside)
		})
	}
}

func TestCollectEndedPartialsRefusesInvalidAge(t *testing.T) {
	f := collectionFixture(t, job.StateFailed)
	for _, age := range []time.Duration{0, -time.Hour} {
		if removed, err := f.p.CollectEndedPartials(context.Background(), collectionTime.Add(100*partialAge), age); err == nil || len(removed) != 0 {
			t.Fatalf("age %v: %v %v", age, removed, err)
		}
	}
	requireKept(t, f.p.store.WorkPath(f.receipt.OperationID))
}
