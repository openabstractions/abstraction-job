package acceptanceprovider

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	job "github.com/openabstractions/abstraction-job/go"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

// Accepted work across an upgrade that fails at each replacement step. The
// retained old payload is the installed executor and profile; the retained
// metadata is the private root. At every step at most one host holds the root,
// and every accepted operation keeps its receipt and result bytes.

func rootSnapshot(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		files[rel] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func sameSnapshot(t *testing.T, step string, before, after map[string][]byte) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("%s changed the file set: %d -> %d", step, len(before), len(after))
	}
	for name, data := range before {
		if !bytes.Equal(after[name], data) {
			t.Fatalf("%s changed %s", step, name)
		}
	}
}

func completeOperation(t *testing.T, p *Provider, id string) {
	t.Helper()
	claim, err := p.store.Claim(id, "upgrade-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.store.Update(claim.ID, claim.Lease.Epoch, func(record *job.Record) error { record.State = job.StateComplete; return nil }); err != nil {
		t.Fatal(err)
	}
}

func readWholeResult(t *testing.T, p *Provider, scope string, identity api.RequestIdentity, receipt api.Receipt) []byte {
	t.Helper()
	var got []byte
	for {
		result, err := p.BindOperations(scope).ReadResult(identity, int64(len(got)), 7)
		if err != nil || result.Chunk == nil {
			t.Fatalf("read result: %+v %v", result, err)
		}
		chunk := result.Chunk
		if chunk.Receipt.OperationId != receipt.OperationId || chunk.Receipt.LogicalOwner != receipt.LogicalOwner || chunk.Offset != int64(len(got)) {
			t.Fatalf("result chunk changed identity or offset: %+v", chunk)
		}
		got = append(got, chunk.Data...)
		if chunk.Eof {
			return got
		}
	}
}

func sameReceipt(t *testing.T, step string, p *Provider, scope string, identity api.RequestIdentity, want api.Receipt) {
	t.Helper()
	v, err := p.Bind(scope).Reconcile(identity)
	if err != nil || v.Receipt == nil || v.Receipt.OperationId != want.OperationId || v.Receipt.LogicalOwner != want.LogicalOwner {
		t.Fatalf("%s changed the receipt: %+v %v", step, v, err)
	}
}

// oneWriter races several acquisitions of the root and requires exactly one to
// win while nobody else holds it, or none while holder is non-nil.
func oneWriter(t *testing.T, root string, held bool) io.Closer {
	t.Helper()
	var mu sync.Mutex
	var winners []io.Closer
	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			guard, err := AcquireHost(root)
			if err != nil {
				if !errors.Is(err, ErrHostActive) {
					t.Error(err)
				}
				return
			}
			mu.Lock()
			winners = append(winners, guard)
			mu.Unlock()
		}()
	}
	wait.Wait()
	if held {
		if len(winners) != 0 {
			t.Fatalf("%d hosts took a root that was already held", len(winners))
		}
		return nil
	}
	if len(winners) != 1 {
		t.Fatalf("%d hosts took one free root", len(winners))
	}
	return winners[0]
}

func TestAcceptedWorkSurvivesFailedUpgradeSteps(t *testing.T) {
	root := t.TempDir()
	installed := resultExecutor{testExecutor: testExecutor{profile: "installed-v1", prefix: "private/"}, body: []byte("result bytes retained across a failed upgrade")}

	// The installed host owns the root and accepts two operations; one completes.
	host := oneWriter(t, root, false)
	p, err := OpenManaged(root, installed)
	if err != nil {
		t.Fatal(err)
	}
	completed, pending := submission(p, "completed-before-upgrade"), submission(p, "pending-during-upgrade")
	completedReceipt := accept(t, p.Bind("app"), completed)
	pendingReceipt := accept(t, p.Bind("app"), pending)
	completeOperation(t, p, completedReceipt.OperationId)
	want := readWholeResult(t, p, "app", completed.Identity, completedReceipt)
	if !bytes.Equal(want, installed.body) {
		t.Fatal("installed result differs from its executor")
	}

	// The incoming package cannot take the root while the installed host drains.
	oneWriter(t, root, true)
	host.Close()
	retained := rootSnapshot(t, root)

	// Failed incoming preflight: an incompatible candidate is refused and writes nothing.
	incoming := testExecutor{profile: "incoming-v2", prefix: "private/"}
	if err := CheckManaged(root, incoming); !errors.Is(err, ErrIncompatibleStorage) {
		t.Fatalf("incompatible candidate preflight: %v", err)
	}
	if _, err := OpenManaged(root, incoming); err == nil {
		t.Fatal("incompatible candidate opened the retained root")
	}
	sameSnapshot(t, "failed preflight", retained, rootSnapshot(t, root))

	// Failed activation: a compatible candidate holds the root, sees the same
	// receipts, and fails before serving. The retained installation cannot open
	// a second writer meanwhile.
	candidateHost := oneWriter(t, root, false)
	candidate, err := OpenManaged(root, installed)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.LogicalOwner() != p.LogicalOwner() {
		t.Fatal("candidate changed the logical owner")
	}
	sameReceipt(t, "candidate", candidate, "app", completed.Identity, completedReceipt)
	sameReceipt(t, "candidate", candidate, "app", pending.Identity, pendingReceipt)
	oneWriter(t, root, true)
	candidateHost.Close()

	// Rollback: the retained payload reopens the retained metadata as the one writer.
	restoredHost := oneWriter(t, root, false)
	defer restoredHost.Close()
	restored, err := OpenManaged(root, installed)
	if err != nil {
		t.Fatal(err)
	}
	if restored.LogicalOwner() != p.LogicalOwner() || restored.config.Epoch != p.config.Epoch {
		t.Fatal("rollback changed the logical owner or history epoch")
	}
	sameReceipt(t, "rollback", restored, "app", completed.Identity, completedReceipt)
	sameReceipt(t, "rollback", restored, "app", pending.Identity, pendingReceipt)
	if got := readWholeResult(t, restored, "app", completed.Identity, completedReceipt); !bytes.Equal(got, want) {
		t.Fatal("rollback changed completed result bytes")
	}
	if duplicate := accept(t, restored.Bind("app"), pending); duplicate.OperationId != pendingReceipt.OperationId {
		t.Fatal("rollback accepted the pending submission a second time")
	}
	completeOperation(t, restored, pendingReceipt.OperationId)
	if got := readWholeResult(t, restored, "app", pending.Identity, pendingReceipt); !bytes.Equal(got, installed.body) {
		t.Fatal("work accepted before the upgrade completed with different bytes")
	}
	if jobs(t, root) != 2 {
		t.Fatalf("upgrade steps created %d operations, want 2", jobs(t, root))
	}
}
