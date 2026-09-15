package acceptanceprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	cas "github.com/openabstractions/abstraction-cas/go"
	job "github.com/openabstractions/abstraction-job/go"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

const legacyProcessExit = "legacy.test/process-exit@1"

var legacyStates = []job.State{job.StatePending, job.StateRunning, job.StateTransferred, job.StateComplete, job.StateFailed, job.StateCancelled}

// legacyExecutor reproduces legacy work unchanged: the submission is the work.
type legacyExecutor struct{}

func (legacyExecutor) Profile() string { return "legacy-migration-test@1" }
func (legacyExecutor) Prepare(_, kind string, spec []byte) ([]byte, error) {
	if kind != "legacy-download" {
		return nil, errors.New("unsupported kind")
	}
	return spec, nil
}
func (legacyExecutor) Serve(ctx context.Context, _ job.Store) error { <-ctx.Done(); return nil }
func (legacyExecutor) ExecutionGuarantees() []string                { return []string{legacyProcessExit} }
func (e legacyExecutor) PrepareWithGuarantees(id, kind string, spec []byte, required []string) ([]byte, []string, error) {
	if !slices.Equal(required, []string{legacyProcessExit}) {
		return nil, nil, errors.New("unsupported guarantees")
	}
	work, err := e.Prepare(id, kind, spec)
	return work, []string{"survives_process_exit"}, err
}
func (e legacyExecutor) PrepareLegacy(id, kind string, spec []byte, required []string) ([]byte, []string, error) {
	return e.PrepareWithGuarantees(id, kind, spec, required)
}

// withoutLegacyPreparation keeps the profile but cannot reproduce migrated work.
type withoutLegacyPreparation struct{ inner legacyExecutor }

func (w withoutLegacyPreparation) Profile() string { return w.inner.Profile() }
func (w withoutLegacyPreparation) Prepare(id, kind string, spec []byte) ([]byte, error) {
	return w.inner.Prepare(id, kind, spec)
}
func (w withoutLegacyPreparation) Serve(ctx context.Context, s job.Store) error {
	return w.inner.Serve(ctx, s)
}
func (w withoutLegacyPreparation) ExecutionGuarantees() []string {
	return w.inner.ExecutionGuarantees()
}
func (w withoutLegacyPreparation) PrepareWithGuarantees(id, kind string, spec []byte, required []string) ([]byte, []string, error) {
	return w.inner.PrepareWithGuarantees(id, kind, spec, required)
}

func (legacyExecutor) ReadOperationResult(root string, record *job.Record, offset, maxBytes int64) ([]byte, int64, error) {
	data, err := os.ReadFile(filepath.Join(root, "results", record.ID))
	if err != nil {
		return nil, 0, err
	}
	if offset > int64(len(data)) {
		return nil, 0, ErrResultRange
	}
	end := min(int64(len(data)), offset+maxBytes)
	return data[offset:end], int64(len(data)), nil
}

// writeLegacyStates creates one real FileStore record per state with retained
// work bytes and checkpoint; active states keep a live lease and delegate handle.
func writeLegacyStates(t *testing.T, root string) map[job.State]string {
	t.Helper()
	legacy, err := job.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[job.State]string{}
	for _, state := range legacyStates {
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
		ids[state] = id
	}
	writeResult(t, root, ids[job.StateComplete])
	return ids
}

func writeResult(t *testing.T, root, id string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "results"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "results", id), []byte("legacy result bytes for "+id), 0600); err != nil {
		t.Fatal(err)
	}
}

func legacyAssignment(t *testing.T, root, id string) LegacyAssignment {
	t.Helper()
	inventory, err := InspectLegacyJobs(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range inventory {
		if item.OperationID == id {
			return LegacyAssignment{OperationID: id, RecordSHA256: item.RecordSHA256, CallerScope: "owner-program@1:caller-of-" + id, Key: "legacy-request-" + id,
				Kind: "legacy-download", Spec: []byte(`{"source":"legacy://opaque"}`), RequiredGuarantees: []string{GuaranteeCallerExit, legacyProcessExit}}
		}
	}
	t.Fatalf("no legacy record %s", id)
	return LegacyAssignment{}
}

func legacyMapping(t *testing.T, root string, ids ...string) LegacyMapping {
	t.Helper()
	var m LegacyMapping
	for _, id := range ids {
		m.Assignments = append(m.Assignments, legacyAssignment(t, root, id))
	}
	return m
}

func allIDs(ids map[job.State]string) []string {
	out := []string{}
	for _, state := range legacyStates {
		if id, ok := ids[state]; ok {
			out = append(out, id)
		}
	}
	return out
}

func assertNoAcceptance(t *testing.T, root string, before map[string]string) {
	t.Helper()
	if !reflect.DeepEqual(before, storageTree(t, root)) {
		t.Fatal("refused migration changed storage")
	}
	if _, err := os.Stat(filepath.Join(root, "acceptance")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused migration created acceptance metadata", err)
	}
	if err := CheckManaged(root, legacyExecutor{}); !errors.Is(err, ErrLegacyOwnership) {
		t.Fatal("managed preflight no longer refuses", err)
	}
}

func journalCount(t *testing.T, root string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "acceptance", "requests"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

func TestLegacyMigrationRefusesActiveUnmappedAndChangedWithoutWrites(t *testing.T) {
	root := t.TempDir()
	ids := writeLegacyStates(t, root)
	inventory, err := InspectLegacyJobs(root)
	if err != nil {
		t.Fatal(err)
	}
	wantInventory := map[string]LegacyReason{ids[job.StatePending]: LegacyNotTerminal, ids[job.StateRunning]: LegacyActiveDelegation,
		ids[job.StateTransferred]: LegacyActiveDelegation, ids[job.StateComplete]: "", ids[job.StateFailed]: "", ids[job.StateCancelled]: ""}
	if len(inventory) != len(wantInventory) {
		t.Fatalf("inventory %+v", inventory)
	}
	for _, item := range inventory {
		if reason, ok := wantInventory[item.OperationID]; !ok || item.Refusal != reason || len(item.RecordSHA256) != 64 {
			t.Fatalf("inventory item %+v", item)
		}
	}
	before := storageTree(t, root)
	active := []LegacyRefusal{{ids[job.StatePending], LegacyNotTerminal}, {ids[job.StateRunning], LegacyActiveDelegation}, {ids[job.StateTransferred], LegacyActiveDelegation}}

	// Mapping active work to a caller never assigns it: the worker or delegate still owns it.
	result, err := MigrateLegacy(root, legacyExecutor{}, legacyMapping(t, root, allIDs(ids)...))
	if !errors.Is(err, ErrLegacyMigrationRefused) || !errors.Is(err, ErrLegacyOwnership) || !errors.Is(err, ErrIncompatibleStorage) {
		t.Fatal("expected typed refusal", err)
	}
	if !sameRefusals(result.Refused, active) || len(result.Converted) != 0 || result.LogicalOwner != "" {
		t.Fatalf("refusal %+v", result)
	}
	assertNoAcceptance(t, root, before)

	changed := legacyAssignment(t, root, ids[job.StateComplete])
	changed.RecordSHA256 = strings.Repeat("0", 64)
	incompatible := legacyAssignment(t, root, ids[job.StateFailed])
	incompatible.Spec = []byte(`{"source":"legacy://different"}`)
	missing := legacyAssignment(t, root, ids[job.StateCancelled])
	missing.OperationID, missing.Key = "legacy-absent", "absent"
	result, err = MigrateLegacy(root, legacyExecutor{}, LegacyMapping{Assignments: []LegacyAssignment{changed, incompatible, missing}})
	want := append(slices.Clone(active),
		LegacyRefusal{ids[job.StateComplete], LegacyRecordChanged},
		LegacyRefusal{ids[job.StateFailed], LegacyIncompatibleWork},
		LegacyRefusal{ids[job.StateCancelled], LegacyUnmapped},
		LegacyRefusal{"legacy-absent", LegacyMappedRecordMissing})
	if !errors.Is(err, ErrLegacyMigrationRefused) || !sameRefusals(result.Refused, want) {
		t.Fatalf("typed refusals %+v %v", result.Refused, err)
	}
	assertNoAcceptance(t, root, before)

	// Admission-only managed storage cannot carry execution requirements.
	terminal := legacyMapping(t, root, ids[job.StateComplete], ids[job.StateFailed], ids[job.StateCancelled])
	if _, err = MigrateLegacy(root, nil, terminal); !errors.Is(err, ErrLegacyMigrationRefused) {
		t.Fatal("admission-only migration accepted execution requirements", err)
	}
	assertNoAcceptance(t, root, before)

	duplicate := legacyMapping(t, root, ids[job.StateComplete], ids[job.StateFailed])
	duplicate.Assignments[1].Key = duplicate.Assignments[0].Key
	duplicate.Assignments[1].CallerScope = duplicate.Assignments[0].CallerScope
	if _, err = MigrateLegacy(root, legacyExecutor{}, duplicate); !errors.Is(err, ErrLegacyMappingInvalid) {
		t.Fatal("duplicate request identity accepted", err)
	}
	if _, err = MigrateLegacy(root, legacyExecutor{}, LegacyMapping{}); !errors.Is(err, ErrLegacyMappingInvalid) {
		t.Fatal("empty mapping accepted", err)
	}
	assertNoAcceptance(t, root, before)

	// The explicit legacy provider still reads every refused record unchanged.
	legacy, err := job.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range allIDs(ids) {
		if _, err := legacy.Load(id); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(before, storageTree(t, root)) {
		t.Fatal("legacy continuity changed storage")
	}
}

func sameRefusals(got, want []LegacyRefusal) bool {
	a, b := slices.Clone(got), slices.Clone(want)
	cmp := func(x, y LegacyRefusal) int {
		return strings.Compare(x.Entry+"\x00"+string(x.Reason), y.Entry+"\x00"+string(y.Reason))
	}
	slices.SortFunc(a, cmp)
	slices.SortFunc(b, cmp)
	return slices.Equal(a, b)
}

// drainLegacy finishes active work through the explicitly selected legacy
// provider: the existing worker completes under its own lease epoch.
func drainLegacy(t *testing.T, root string, ids map[job.State]string) {
	t.Helper()
	legacy, err := job.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = legacy.Update(ids[job.StateRunning], 17, func(r *job.Record) error { r.State = job.StateComplete; r.Progress.Done = 19; return nil }); err != nil {
		t.Fatal(err)
	}
	writeResult(t, root, ids[job.StateRunning])
	if _, err = legacy.Update(ids[job.StateTransferred], 17, func(r *job.Record) error {
		r.State = job.StateFailed
		r.Error = "delegate reported failure"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := legacy.Claim(ids[job.StatePending], "legacy-drainer", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = legacy.Update(claimed.ID, claimed.Lease.Epoch, func(r *job.Record) error { r.State = job.StateCancelled; return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyMigrationConvertsDrainedStatesKeepingBytesAndResults(t *testing.T) {
	root := t.TempDir()
	ids := writeLegacyStates(t, root)
	drainLegacy(t, root, ids)
	mapping := legacyMapping(t, root, allIDs(ids)...)
	legacy, err := job.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	records := map[string]*job.Record{}
	for _, id := range allIDs(ids) {
		if records[id], err = legacy.Load(id); err != nil {
			t.Fatal(err)
		}
	}
	before := storageTree(t, root)

	result, err := MigrateLegacy(root, legacyExecutor{}, mapping)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.LogicalOwner, "openabstractions.job/") || result.HistoryEpoch == "" || len(result.Converted) != 6 || len(result.Refused) != 0 || result.AlreadyMigrated {
		t.Fatalf("migration %+v", result)
	}
	after := storageTree(t, root)
	for name, data := range before {
		if after[name] != data {
			t.Fatalf("migration changed %s", name)
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok && !strings.HasPrefix(name, "acceptance") {
			t.Fatalf("migration wrote outside acceptance metadata: %s", name)
		}
	}
	if journalCount(t, root) != 6 {
		t.Fatal("expected one journal per converted record")
	}

	p, err := OpenManaged(root, legacyExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.CloseInventory()
	if p.LogicalOwner() != result.LogicalOwner || p.config.Epoch != result.HistoryEpoch {
		t.Fatal("managed open changed migrated identity")
	}
	for _, a := range mapping.Assignments {
		id := api.RequestIdentity{Key: a.Key, HistoryEpoch: result.HistoryEpoch}
		record := records[a.OperationID]
		observed, err := p.BindOperations(a.CallerScope).ObserveWork(id)
		if err != nil || observed.Outcome != "observed" || observed.Snapshot.Receipt.OperationId != a.OperationID || observed.Snapshot.State != string(record.State) ||
			observed.Snapshot.Progress.Done != record.Progress.Done || observed.Snapshot.Progress.Total != record.Progress.Total {
			t.Fatalf("observe %s: %+v %v", a.OperationID, observed, err)
		}
		reconciled, err := p.Bind(a.CallerScope).Reconcile(id)
		if err != nil || reconciled.Outcome != "accepted" || reconciled.Receipt.OperationId != a.OperationID || reconciled.Receipt.LogicalOwner != result.LogicalOwner {
			t.Fatalf("reconcile %s: %+v %v", a.OperationID, reconciled, err)
		}
		if other, err := p.BindOperations("owner-program@1:someone-else").ObserveWork(id); err != nil || other.Outcome != "unknown" {
			t.Fatalf("another caller observed migrated work: %+v %v", other, err)
		}
		read, err := p.BindOperations(a.CallerScope).ReadResult(id, 0, MaxResultBytes)
		switch record.State {
		case job.StateComplete:
			want, _ := os.ReadFile(filepath.Join(root, "results", a.OperationID))
			if err != nil || read.Outcome != "data" || string(read.Chunk.Data) != string(want) || !read.Chunk.Eof {
				t.Fatalf("result %s: %+v %v", a.OperationID, read, err)
			}
		default:
			if err != nil || read.Outcome != "unavailable" {
				t.Fatalf("terminal non-result %s: %+v %v", a.OperationID, read, err)
			}
		}
		page, err := p.BindInventory(a.CallerScope).ListWork("", 64)
		if err != nil || !page.Complete || len(page.Snapshots) != 1 || page.Snapshots[0].Receipt.OperationId != a.OperationID {
			t.Fatalf("inventory %s: %+v %v", a.OperationID, page, err)
		}
		reopened, err := legacy.Load(a.OperationID)
		if err != nil || !reflect.DeepEqual(reopened, record) {
			t.Fatal("explicit legacy access changed after migration", err)
		}
	}
	for name, data := range before {
		if storageTree(t, root)[name] != data {
			t.Fatalf("service access changed legacy bytes %s", name)
		}
	}

	// Repeating the same migration is idempotent and writes nothing.
	settled := storageTree(t, root)
	again, err := MigrateLegacy(root, legacyExecutor{}, mapping)
	if err != nil || !again.AlreadyMigrated || again.LogicalOwner != result.LogicalOwner || again.HistoryEpoch != result.HistoryEpoch || len(again.Converted) != 6 {
		t.Fatalf("repeat %+v %v", again, err)
	}
	// A different owner claim for converted work cannot create a second owner.
	different := legacyMapping(t, root, allIDs(ids)...)
	different.Assignments[0].CallerScope = "owner-program@1:next-caller"
	if _, err = MigrateLegacy(root, legacyExecutor{}, different); !errors.Is(err, ErrLegacyMigrationConflict) {
		t.Fatal("second mapping accepted", err)
	}
	if _, err = MigrateLegacy(root, testExecutor{profile: "other-profile@1"}, mapping); !errors.Is(err, ErrLegacyMigrationConflict) {
		t.Fatal("different execution profile accepted", err)
	}
	if !reflect.DeepEqual(settled, storageTree(t, root)) || journalCount(t, root) != 6 {
		t.Fatal("repeated or conflicting migration changed storage")
	}
	if err = CheckManaged(root, legacyExecutor{}); err != nil {
		t.Fatal(err)
	}
	if profile, owned, err := RecordedExecutionProfile(root); err != nil || !owned || profile != (legacyExecutor{}).Profile() {
		t.Fatal("recorded profile", profile, owned, err)
	}
	// Migrated journals are recoverable only through legacy preparation, and a
	// new submission never receives that origin.
	if _, err = OpenManaged(root, withoutLegacyPreparation{}); err == nil {
		t.Fatal("executor without legacy preparation recovered migrated work")
	}
	fresh, err := OpenManaged(root, legacyExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.CloseInventory()
	s := api.Submission{Identity: api.RequestIdentity{Key: "new-after-migration", HistoryEpoch: result.HistoryEpoch}, Kind: "legacy-download", Spec: []byte(`{"source":"new"}`), RequiredGuarantees: []string{legacyProcessExit}}
	accepted, err := fresh.Bind("owner-program@1:new-caller").Submit(s)
	if err != nil || accepted.Outcome != "accepted" {
		t.Fatal(accepted, err)
	}
	var admitted journal
	data, err := os.ReadFile(fresh.requestPath("owner-program@1:new-caller", s.Identity))
	if err != nil || json.Unmarshal(data, &admitted) != nil || admitted.Origin != "" {
		t.Fatal("new submission carried a legacy origin", admitted.Origin, err)
	}
}

func TestLegacyMigrationFencesLivePredecessorAndConcurrentMigrators(t *testing.T) {
	terminalRoot := func(t *testing.T) (string, []string) {
		root := t.TempDir()
		ids := writeLegacyStates(t, root)
		drainLegacy(t, root, ids)
		return root, allIDs(ids)
	}

	t.Run("live-host", func(t *testing.T) {
		root, ids := terminalRoot(t)
		lease, err := AcquireHost(root)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = MigrateLegacy(root, legacyExecutor{}, legacyMapping(t, root, ids...)); !errors.Is(err, ErrHostActive) {
			t.Fatal("migration ran beside a live host", err)
		}
		lease.Close()
		for _, name := range []string{"owner.json", legacyMigrationFile, "requests"} {
			if _, err := os.Stat(filepath.Join(root, "acceptance", name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("fenced migration wrote", name)
			}
		}
	})

	t.Run("predecessor-rewrote-record", func(t *testing.T) {
		root, ids := terminalRoot(t)
		mapping := legacyMapping(t, root, ids...)
		path := filepath.Join(root, "jobs", ids[3]+".json")
		old, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		record, err := job.Decode(old)
		if err != nil {
			t.Fatal(err)
		}
		record.Error = "late predecessor write"
		rewritten, err := record.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if err = cas.Write(path, old, rewritten); err != nil {
			t.Fatal(err)
		}
		before := storageTree(t, root)
		result, err := MigrateLegacy(root, legacyExecutor{}, mapping)
		if !errors.Is(err, ErrLegacyMigrationRefused) || !sameRefusals(result.Refused, []LegacyRefusal{{ids[3], LegacyRecordChanged}}) {
			t.Fatalf("changed record converted: %+v %v", result, err)
		}
		assertNoAcceptance(t, root, before)
	})

	t.Run("interrupted-then-resumed", func(t *testing.T) {
		root, ids := terminalRoot(t)
		mapping := legacyMapping(t, root, ids...)
		journals := 0
		legacyMigrationFault = func(point string) error {
			if point == "after-journal" {
				journals++
				if journals == 2 {
					return errors.New("interrupted")
				}
			}
			return nil
		}
		_, err := MigrateLegacy(root, legacyExecutor{}, mapping)
		legacyMigrationFault = nil
		if err == nil || journalCount(t, root) != 2 {
			t.Fatal("interruption not reached", err)
		}
		if _, err = os.Stat(filepath.Join(root, "acceptance", "owner.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("partial migration configured an owner", err)
		}
		if _, err = OpenManaged(root, legacyExecutor{}); !errors.Is(err, ErrLegacyOwnership) {
			t.Fatal("managed open accepted a partial migration", err)
		}
		different := legacyMapping(t, root, ids...)
		different.Assignments[5].Key = "a-different-request"
		if _, err = MigrateLegacy(root, legacyExecutor{}, different); !errors.Is(err, ErrLegacyMigrationConflict) {
			t.Fatal("different mapping replaced a recorded migration", err)
		}
		var recorded migrationRecord
		data, err := os.ReadFile(filepath.Join(root, "acceptance", legacyMigrationFile))
		if err != nil || json.Unmarshal(data, &recorded) != nil {
			t.Fatal("recorded migration unreadable", err)
		}
		result, err := MigrateLegacy(root, legacyExecutor{}, mapping)
		if err != nil || result.LogicalOwner != recorded.Owner || result.HistoryEpoch != recorded.Epoch || journalCount(t, root) != 6 {
			t.Fatalf("resume %+v %v", result, err)
		}
		p, err := OpenManaged(root, legacyExecutor{})
		if err != nil || p.LogicalOwner() != recorded.Owner {
			t.Fatal("resumed owner not retained", err)
		}
		p.CloseInventory()
	})

	t.Run("predecessor-writes-during-commit", func(t *testing.T) {
		root, ids := terminalRoot(t)
		mapping := legacyMapping(t, root, ids...)
		before := storageTree(t, root)
		path := filepath.Join(root, "jobs", ids[4]+".json")
		var rewritten string
		legacyMigrationFault = func(point string) error {
			if point != "after-plan" {
				return nil
			}
			old, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			record, err := job.Decode(old)
			if err != nil {
				return err
			}
			record.Error = "predecessor finished writing late"
			next, err := record.Encode()
			rewritten = string(next)
			if err != nil {
				return err
			}
			return cas.Write(path, old, next)
		}
		_, err := MigrateLegacy(root, legacyExecutor{}, mapping)
		legacyMigrationFault = nil
		if !errors.Is(err, errLegacyRecordMoved) {
			t.Fatal("record moved under the migration lock was converted", err)
		}
		if _, err = os.Stat(filepath.Join(root, "acceptance", "owner.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("moved record left a configured owner", err)
		}
		if _, err = OpenManaged(root, legacyExecutor{}); !errors.Is(err, ErrLegacyOwnership) {
			t.Fatal("managed open accepted an interrupted migration", err)
		}
		if err = AbandonLegacyMigration(root); err != nil {
			t.Fatal(err)
		}
		after := storageTree(t, root)
		for name, data := range before {
			want := data
			if name == filepath.Join("jobs", ids[4]+".json") {
				want = rewritten
			}
			if after[name] != want {
				t.Fatalf("abandon changed legacy data %s", name)
			}
		}
		if journalCount(t, root) != 0 {
			t.Fatal("abandon retained migration journals")
		}
		if _, err = os.Stat(filepath.Join(root, "acceptance", legacyMigrationFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("abandon retained the recorded mapping", err)
		}
		result, err := MigrateLegacy(root, legacyExecutor{}, legacyMapping(t, root, ids...))
		if err != nil || len(result.Converted) != 6 || journalCount(t, root) != 6 {
			t.Fatalf("re-reviewed mapping %+v %v", result, err)
		}
		if err = AbandonLegacyMigration(root); !errors.Is(err, ErrLegacyMigrationConflict) {
			t.Fatal("completed migration abandoned", err)
		}
	})

	t.Run("concurrent-migrators", func(t *testing.T) {
		root, ids := terminalRoot(t)
		first := legacyMapping(t, root, ids...)
		second := legacyMapping(t, root, ids...)
		for i := range second.Assignments {
			second.Assignments[i].CallerScope = "owner-program@1:competing-operator"
		}
		type outcome struct {
			mapping int
			result  LegacyMigration
			err     error
		}
		start, outcomes := make(chan struct{}), make(chan outcome, 16)
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				m := first
				if i%2 == 1 {
					m = second
				}
				<-start
				r, err := MigrateLegacy(root, legacyExecutor{}, m)
				outcomes <- outcome{i % 2, r, err}
			}(i)
		}
		close(start)
		wg.Wait()
		close(outcomes)
		owners := map[string]bool{}
		winners := map[int]bool{}
		for o := range outcomes {
			switch {
			case o.err == nil:
				owners[o.result.LogicalOwner+"/"+o.result.HistoryEpoch] = true
				winners[o.mapping] = true
			case errors.Is(o.err, ErrHostActive), errors.Is(o.err, ErrLegacyMigrationConflict):
			default:
				t.Fatal("unexpected concurrent result", o.err)
			}
		}
		if len(owners) > 1 || len(winners) > 1 {
			t.Fatal("concurrent migrations created more than one owner", owners, winners)
		}
		// Whichever operator won (or none, if every attempt met the host lease),
		// one retry settles the root, and the other mapping is refused.
		settledResult, errFirst := MigrateLegacy(root, legacyExecutor{}, first)
		_, errSecond := MigrateLegacy(root, legacyExecutor{}, second)
		if (errFirst == nil) == (errSecond == nil) {
			t.Fatal("exactly one mapping must own the root", errFirst, errSecond)
		}
		if errFirst != nil {
			settledResult, _ = MigrateLegacy(root, legacyExecutor{}, second)
		}
		if len(owners) == 1 && !owners[settledResult.LogicalOwner+"/"+settledResult.HistoryEpoch] {
			t.Fatal("retry changed the winning owner")
		}
		if journalCount(t, root) != 6 {
			t.Fatal(fmt.Sprintf("expected six journals, found %d", journalCount(t, root)))
		}
	})
}
