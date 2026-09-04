package job

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func openStore(t *testing.T) *FileStore {
	t.Helper()
	s, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func submitOne(t *testing.T, s Store) string {
	t.Helper()
	var r Record
	if err := r.SetSpec(map[string]string{"what": "anything"}); err != nil {
		t.Fatal(err)
	}
	r.Kind = "test"
	id, err := s.Submit(r)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// The interface is worth nothing if the only implementation does not satisfy it
// through the interface type, so this exercises it as a Store and never as a
// *FileStore.
func TestHandleCancelsThroughTheInterface(t *testing.T) {
	var s Store = openStore(t)
	id := submitOne(t, s)

	j := Open(s, id, "test-owner")
	if j.ID() != id {
		t.Fatalf("handle id = %q, want %q", j.ID(), id)
	}
	if err := j.Cancel(); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	r, err := j.Record()
	if err != nil {
		t.Fatal(err)
	}
	if r.State != StateCancelled {
		t.Fatalf("state = %q, want %q", r.State, StateCancelled)
	}
	// Idempotent: a UI that double-clicks must not produce an error.
	if err := j.Cancel(); err != nil {
		t.Fatalf("second cancel: %v", err)
	}
}

// The case the whole of schema 4 exists for: somebody who holds no lease, and
// never will, stops a job that another process is working on.
//
// This test previously asserted the opposite — that cancelling was REFUSED here.
// That refusal was a limitation being documented, not a behaviour worth keeping.
func TestCancelReachesAJobSomebodyElseHolds(t *testing.T) {
	var s Store = openStore(t)
	id := submitOne(t, s)

	if _, err := s.Claim(id, "somebody-else", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := Open(s, id, "a-person").Cancel(); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	r, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if r.Wants() != WantCancel {
		t.Fatalf("wants %q, want %q", r.Wants(), WantCancel)
	}
	// The owner still holds the lease and the state is still theirs to change.
	// Desired is not observed, and pretending otherwise here would be two
	// writers again.
	if r.State == StateCancelled {
		t.Fatal("state was changed under a live owner; only the owner may do that")
	}
	if r.Intent.By != "a-person" {
		t.Fatalf("intent recorded by %q — who asked is part of the record", r.Intent.By)
	}
}

// The file binding implements Pausable, because a checkpoint is all pausing
// needs. An application discovers that by asking.
func TestFileBindingIsPausable(t *testing.T) {
	var s Store = openStore(t)
	j := Open(s, submitOne(t, s), "test-owner")
	p, ok := j.(Pausable)
	if !ok {
		t.Fatal("the file binding should be Pausable — it has a durable checkpoint")
	}
	if err := p.Pause(); err != nil {
		t.Fatalf("pause: %v", err)
	}
	r, _ := j.Record()
	if r.Wants() != WantPause || !r.Paused() {
		t.Fatalf("wants %q, paused %v", r.Wants(), r.Paused())
	}
	if err := p.Resume(); err != nil {
		t.Fatalf("resume: %v", err)
	}
	r, _ = j.Record()
	if r.Wants() != WantRun || r.Paused() {
		t.Fatalf("after resume: wants %q, paused %v", r.Wants(), r.Paused())
	}
}

// A paused job looks abandoned and is not. A sweep that adopted it would restart
// the work seconds after a person stopped it — the same trap TRANSFERRED set.
func TestAPausedJobIsNotAnOrphan(t *testing.T) {
	var s Store = openStore(t)
	id := submitOne(t, s)

	orphans, err := s.Orphans()
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 {
		t.Fatalf("%d orphans before pausing, want 1", len(orphans))
	}

	if _, err := s.SetIntent(id, WantPause, "a-person"); err != nil {
		t.Fatal(err)
	}
	orphans, err = s.Orphans()
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Fatalf("%d orphans after pausing, want 0 — a sweep would resume it", len(orphans))
	}
}

// Intent needs no lease. That exemption IS the feature.
func TestSetIntentNeedsNoLease(t *testing.T) {
	var s Store = openStore(t)
	id := submitOne(t, s)
	if _, err := s.Claim(id, "the-worker", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetIntent(id, WantPause, "a-bystander"); err != nil {
		t.Fatalf("setting intent required a lease: %v", err)
	}
}

// Records written before schema 4 must still be continuable. Refusing them would
// orphan work sitting on real disks, including a NAS.
func TestVersionThreeRecordsAreStillReadable(t *testing.T) {
	v3 := []byte(`{
  "schema": 3,
  "id": "1700000000000-abc",
  "kind": "test",
  "state": "running",
  "spec": {"what": "anything"},
  "progress": {"done": 0, "updated_at": "2026-01-01T00:00:00.000000Z"},
  "lease": {"owner": "", "epoch": 0, "expires_at": "2026-01-01T00:00:00.000000Z"},
  "created_at": "2026-01-01T00:00:00.000000Z",
  "updated_at": "2026-01-01T00:00:00.000000Z"
}`)
	r, err := Decode(v3)
	if err != nil {
		t.Fatalf("a version 3 record must still be readable: %v", err)
	}
	// Absent intent means run, which is exactly what version 3 always meant. So
	// nothing is being guessed at.
	if r.Wants() != WantRun {
		t.Fatalf("wants %q, want %q", r.Wants(), WantRun)
	}
	// And what we write, we write as our own version.
	b, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	// Read as legacy, written back in the current form: the integer is gone and
	// the record says what it actually contains. Version 3 carried no intent, so
	// the content set is base alone.
	if strings.Contains(string(b), `"schema"`) {
		t.Fatal("the legacy version integer must not be written back out")
	}
	if want := fmt.Sprintf(`"%s"`, ModelBase); !strings.Contains(string(b), want) {
		t.Fatalf("a record written by this implementation must declare %s", want)
	}
}

// Scratch is optional, and the file binding is the one that has it.
func TestFileBindingAdvertisesScratch(t *testing.T) {
	var s Store = openStore(t)
	sc, ok := s.(Scratch)
	if !ok {
		t.Fatal("file binding should advertise Scratch")
	}
	if sc.Root() == "" || sc.WorkPath("abc") == "" {
		t.Fatal("Scratch returned empty locations")
	}
}
