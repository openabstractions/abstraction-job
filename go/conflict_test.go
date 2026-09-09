package job

import (
	"errors"
	"testing"
	"time"
)

// Two writers holding ONE lease, each changing a different field.
//
// This is legitimate and common: a worker reports progress while a checkpointer
// records what it has proven, both inside the process that claimed the job, both
// presenting the same epoch. The epoch does not move when a holder writes, so
// nothing about the lease distinguishes these two writes from one.
//
// The file and memory bindings run the caller's mutation at the store, under the
// lock, against the record as it stands — so the second writer's mutation is
// applied to the first writer's result and neither is lost. The service binding
// cannot ship a closure, so it reads, mutates its own copy and sends the whole
// record back; if the record moved in between, the fields it did not touch are
// written back at the values it read.
//
// The barrier is the interleave, and it is exact rather than likely: the first
// writer is stopped between its read and its write until the second writer has
// completed.
func TestOneLeaseTwoWritersLoseNothing(t *testing.T) {
	behind, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := listen(t)
	if err != nil {
		t.Fatalf("cannot listen: %v", err)
	}
	go Serve(ln, behind)
	t.Cleanup(func() { ln.Close() })

	reporter := NewRemoteStore(ln.Addr().Network(), ln.Addr().String())
	checkpointer := NewRemoteStore(ln.Addr().Network(), ln.Addr().String())

	var r Record
	r.Kind = "test"
	if err := r.SetSpec(map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}
	id, err := reporter.Submit(r)
	if err != nil {
		t.Fatal(err)
	}
	held, err := reporter.Claim(id, "one-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	epoch := held.Lease.Epoch

	read := make(chan struct{})
	other := make(chan struct{})
	reported := make(chan error, 1)

	go func() {
		_, err := reporter.Update(id, epoch, func(rec *Record) error {
			select {
			case <-read:
			default:
				close(read)
				<-other
			}
			rec.Progress.Done = 400
			return nil
		})
		reported <- err
	}()

	<-read
	if _, err := checkpointer.Update(id, epoch, func(rec *Record) error {
		return rec.SetCheckpoint(map[string]int64{"verified_prefix": 900})
	}); err != nil {
		t.Fatalf("the checkpointer holds the lease and was refused: %v", err)
	}
	close(other)

	if err := <-reported; err != nil {
		t.Fatalf("the reporter holds the lease and was refused: %v", err)
	}

	final, err := behind.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if final.Progress.Done != 400 {
		t.Errorf("progress %d, want 400: the reporter's write was lost", final.Progress.Done)
	}
	var cp struct {
		VerifiedPrefix int64 `json:"verified_prefix"`
	}
	if err := final.DecodeCheckpoint(&cp); err != nil || cp.VerifiedPrefix != 900 {
		t.Errorf("checkpoint %+v (%v), want 900 proven: the checkpointer's write was lost", cp, err)
	}
}

// A write that cannot be reconciled is refused by name, so a caller can tell a
// record it lost a race for from a record it no longer owns. ErrStaleEpoch is
// the second answer and it is a different question.
func TestARemoteWriteOnAMovedRecordIsRefusedRatherThanApplied(t *testing.T) {
	behind, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := listen(t)
	if err != nil {
		t.Fatalf("cannot listen: %v", err)
	}
	go Serve(ln, behind)
	t.Cleanup(func() { ln.Close() })

	client := NewRemoteStore(ln.Addr().Network(), ln.Addr().String())
	var r Record
	r.Kind = "test"
	if err := r.SetSpec(map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}
	id, err := client.Submit(r)
	if err != nil {
		t.Fatal(err)
	}
	held, err := client.Claim(id, "one-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	base, err := client.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := behind.Update(id, held.Lease.Epoch, func(rec *Record) error {
		rec.Progress.Done = 7
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	base.Error = "written against a record that has since moved"
	stale, err := base.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.write(id, held.Lease.Epoch, stale, stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("a write on a moved record got %v, want ErrConflict", err)
	}
	after, err := behind.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Progress.Done != 7 || after.Error != "" {
		t.Fatalf("the refused write landed anyway: progress %d error %q", after.Progress.Done, after.Error)
	}
}
