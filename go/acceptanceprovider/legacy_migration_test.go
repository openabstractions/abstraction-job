package acceptanceprovider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	job "github.com/openabstractions/abstraction-job/go"
)

type migrationExecutor struct{ prepared, served int }

func (*migrationExecutor) Profile() string { return "migration-refusal-test@1" }
func (e *migrationExecutor) Prepare(string, string, []byte) ([]byte, error) {
	e.prepared++
	return nil, errors.New("unexpected preparation")
}
func (e *migrationExecutor) Serve(context.Context, job.Store) error {
	e.served++
	return errors.New("unexpected execution")
}

func TestLegacyRecordsRetainExplicitProviderContinuity(t *testing.T) {
	for _, state := range []job.State{job.StatePending, job.StateRunning, job.StateTransferred, job.StateComplete, job.StateFailed, job.StateCancelled} {
		t.Run(string(state), func(t *testing.T) {
			root := t.TempDir()
			legacy, err := job.NewFileStore(root)
			if err != nil {
				t.Fatal(err)
			}
			record := job.Record{ID: "legacy-" + string(state), Kind: "legacy-download", State: state, Spec: json.RawMessage(`{"source":"legacy://opaque"}`), Checkpoint: json.RawMessage(`{"prefix":7,"ranges":[[0,7]]}`), Progress: job.Progress{Done: 7, Total: 19}, Requires: []string{"survives_process_exit"}}
			if state == job.StateRunning || state == job.StateTransferred {
				record.Lease = job.Lease{Owner: "existing-worker", Epoch: 17, ExpiresAt: job.At(time.Now().Add(time.Hour))}
				record.Delegation = &job.Delegation{System: "existing-external-provider", ExternalID: "retained-handoff-id"}
			}
			id, err := legacy.Submit(record)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(legacy.WorkPath(id), []byte("partial"), 0600); err != nil {
				t.Fatal(err)
			}
			retained, err := legacy.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			before := storageTree(t, root)
			executor := &migrationExecutor{}
			checks := []struct {
				name string
				call func() error
			}{
				{"preflight", func() error { return CheckManaged(root, executor) }},
				{"managed-admission", func() error { _, e := OpenManaged(root, nil); return e }},
				{"managed-execution", func() error { _, e := OpenManaged(root, executor); return e }},
				{"explicit-execution-owner", func() error { _, e := OpenWithExecutor(root, "new-owner", executor); return e }},
			}
			for _, check := range checks {
				t.Run(check.name, func(t *testing.T) {
					err := check.call()
					if !errors.Is(err, ErrLegacyOwnership) || !errors.Is(err, ErrIncompatibleStorage) {
						t.Fatalf("expected ownership refusal, got %v", err)
					}
					if !reflect.DeepEqual(before, storageTree(t, root)) {
						t.Fatal("refusal changed legacy storage or created acceptance metadata")
					}
				})
			}
			if executor.prepared != 0 || executor.served != 0 {
				t.Fatal("refusal reached executor")
			}
			reopened, err := job.NewFileStore(root)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := reopened.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(retained, actual) {
				t.Fatal("explicit legacy access lost identity/checkpoint/ownership")
			}
			if !reflect.DeepEqual(before, storageTree(t, root)) {
				t.Fatal("legacy reopen changed data")
			}
			if _, err = os.Stat(filepath.Join(root, "acceptance")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("fabricated acceptance ownership", err)
			}
		})
	}
}
