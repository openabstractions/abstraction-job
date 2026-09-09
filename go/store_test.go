package job

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// clock lets the lease tests run in microseconds instead of waiting out real
// timeouts. Sleep-based lease tests are slow and flaky, and a flaky test on the
// one mechanism that prevents two owners working the same job is worse than no
// test at all.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newTestStore(t *testing.T) (*FileStore, *clock) {
	t.Helper()
	s, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)}
	s.now = c.now
	return s, c
}

// testSpec stands in for whatever a Kind puts in the opaque spec. This package
// must never need to know what is in here, and the fact that a made-up shape
// works as well as the real one is the property being tested.
type testSpec struct {
	What  string   `json:"what"`
	Where []string `json:"where"`
}

type testCheckpoint struct {
	Proven int64 `json:"proven"`
}

func sampleRecord() Record {
	r := Record{Kind: "test-kind"}
	r.SetSpec(testSpec{What: "a thing", Where: []string{`\\nas\share\thing`, "D:/thing"}})
	return r
}

func TestSubmitAndLoad(t *testing.T) {
	s, _ := newTestStore(t)
	id, err := s.Submit(sampleRecord())
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != StatePending {
		t.Fatalf("state = %q, want pending", r.State)
	}
	if r.Kind != "test-kind" {
		t.Fatalf("kind = %q", r.Kind)
	}
	// The point of the id: it is a plain string another program can hold.
	if strings.ContainsAny(id, `/\`) {
		t.Fatalf("id %q is not safe to pass around as an opaque token", id)
	}
}

// TestSpecIsOpaque is the assertion behind schema 3. This package stores and
// returns the spec without understanding it, so a Kind can change what it puts
// there — mirrors, chunk manifests, whatever downloading needs next — without
// forcing a schema change on every language that reads these records.
func TestSpecIsOpaque(t *testing.T) {
	s, _ := newTestStore(t)
	// A shape this package has never seen, with nesting it has no types for.
	raw := json.RawMessage(`{"unheard_of":{"deeply":["nested",{"values":42}]},"n":7}`)
	id, err := s.Submit(Record{Kind: "some-future-kind", Spec: raw})
	if err != nil {
		t.Fatalf("an unfamiliar spec was rejected: %v", err)
	}
	r, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := r.DecodeSpec(&got); err != nil {
		t.Fatal(err)
	}
	if got["n"].(float64) != 7 {
		t.Fatalf("spec did not survive the round trip: %v", got)
	}
}

func TestKindIsRequired(t *testing.T) {
	s, _ := newTestStore(t)
	r := sampleRecord()
	r.Kind = ""
	if _, err := s.Submit(r); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Submit without a kind = %v; an opaque spec nobody can identify is unusable", err)
	}
}

func TestSpecIsRequired(t *testing.T) {
	s, _ := newTestStore(t)
	if _, err := s.Submit(Record{Kind: "test-kind"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Submit with no spec = %v, want ErrInvalid", err)
	}
}

func TestClaimIsExclusive(t *testing.T) {
	s, _ := newTestStore(t)
	id, _ := s.Submit(sampleRecord())

	first, err := s.Claim(id, "go-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if first.Lease.Epoch != 1 || first.State != StateRunning {
		t.Fatalf("first claim: epoch %d state %s", first.Lease.Epoch, first.State)
	}
	if _, err := s.Claim(id, "python-worker", time.Minute); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("second claim = %v, want ErrLeaseHeld", err)
	}
}

// TestOrphanIsAdoptedAfterExpiry is the SIGKILL case in miniature. The first
// owner never releases anything — it simply stops existing — and the job must
// still become available, carrying the checkpoint it had proven.
func TestOrphanIsAdoptedAfterExpiry(t *testing.T) {
	s, c := newTestStore(t)
	id, _ := s.Submit(sampleRecord())

	held, err := s.Claim(id, "go-worker", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(id, held.Lease.Epoch, func(r *Record) error {
		r.Progress.Done = 400
		return r.SetCheckpoint(testCheckpoint{Proven: 400})
	}); err != nil {
		t.Fatal(err)
	}

	if orphans, _ := s.Orphans(); len(orphans) != 0 {
		t.Fatal("a live job was reported as an orphan")
	}

	c.add(31 * time.Second)

	orphans, _ := s.Orphans()
	if len(orphans) != 1 || orphans[0].ID != id {
		t.Fatalf("orphan not found after lease expiry: %+v", orphans)
	}
	adopted, err := s.Claim(id, "python-worker", time.Minute)
	if err != nil {
		t.Fatalf("adopting an orphan failed: %v", err)
	}
	if adopted.Lease.Epoch != 2 {
		t.Fatalf("epoch = %d, want 2", adopted.Lease.Epoch)
	}
	// The successor inherits what the predecessor proved. This is Temporal's
	// heartbeat-details design: a retried worker is handed the dead worker's
	// last checkpoint rather than starting over.
	var cp testCheckpoint
	if err := adopted.DecodeCheckpoint(&cp); err != nil {
		t.Fatal(err)
	}
	if cp.Proven != 400 {
		t.Fatalf("checkpoint = %d, want 400 — the new owner lost the predecessor's proven work", cp.Proven)
	}
}

// TestZombieOwnerIsRefused is the reason the epoch exists. A process suspended
// past its lease wakes up believing it still owns the job. By then someone else
// has claimed it, and the zombie's writes must not land.
func TestZombieOwnerIsRefused(t *testing.T) {
	s, c := newTestStore(t)
	id, _ := s.Submit(sampleRecord())

	zombie, _ := s.Claim(id, "go-worker", 30*time.Second)
	c.add(31 * time.Second) // the machine slept
	if _, err := s.Claim(id, "python-worker", time.Minute); err != nil {
		t.Fatal(err)
	}

	_, err := s.Update(id, zombie.Lease.Epoch, func(r *Record) error {
		r.Progress.Done = 999
		return nil
	})
	if !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("zombie write = %v, want ErrStaleEpoch", err)
	}
	if _, err := s.Renew(id, zombie.Lease.Epoch, time.Minute); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("zombie renew = %v, want ErrStaleEpoch", err)
	}
}

// TestExpiredOwnerCannotRenew covers the subtler half: the lease expired but
// nobody else has claimed yet, so the epoch still matches. Renewing here would
// let an owner asleep for an hour carry on as though nothing happened, while a
// claimant may be a microsecond away from taking over.
func TestExpiredOwnerCannotRenew(t *testing.T) {
	s, c := newTestStore(t)
	id, _ := s.Submit(sampleRecord())
	held, _ := s.Claim(id, "go-worker", 30*time.Second)

	c.add(31 * time.Second)

	if _, err := s.Renew(id, held.Lease.Epoch, time.Minute); !errors.Is(err, ErrLeaseExpiry) {
		t.Fatalf("renew after expiry = %v, want ErrLeaseExpiry", err)
	}
	if _, err := s.Update(id, held.Lease.Epoch, func(r *Record) error { return nil }); !errors.Is(err, ErrLeaseExpiry) {
		t.Fatalf("write after expiry = %v, want ErrLeaseExpiry", err)
	}
	again, err := s.Claim(id, "go-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if again.Lease.Epoch != 2 {
		t.Fatalf("re-claim epoch = %d, want 2", again.Lease.Epoch)
	}
}

// TestReleaseHandsOffImmediately covers the polite path: an optimisation over
// waiting for expiry, never the only way a job moves on.
func TestReleaseHandsOffImmediately(t *testing.T) {
	s, _ := newTestStore(t)
	id, _ := s.Submit(sampleRecord())
	held, _ := s.Claim(id, "go-worker", time.Hour)

	if err := s.Release(id, held.Lease.Epoch); err != nil {
		t.Fatal(err)
	}
	// No clock movement at all: the whole point is not waiting an hour.
	next, err := s.Claim(id, "python-worker", time.Minute)
	if err != nil {
		t.Fatalf("claim after release failed: %v", err)
	}
	if next.Lease.Epoch != 2 {
		t.Fatalf("epoch = %d, want 2", next.Lease.Epoch)
	}
}

// A claim leaves the record and its lock beside it and nothing else: the lock
// is the whole of the exclusion, so nothing accumulates per claim.
func TestAClaimLeavesOnlyTheRecordAndItsLock(t *testing.T) {
	s, c := newTestStore(t)
	id, _ := s.Submit(sampleRecord())
	for i := 1; i <= 3; i++ {
		r, err := s.Claim(id, "worker", time.Second)
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if r.Lease.Epoch != int64(i) {
			t.Fatalf("claim %d gave epoch %d", i, r.Lease.Epoch)
		}
		c.add(2 * time.Second)
	}
	entries, err := os.ReadDir(filepath.Join(s.Root(), "jobs"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if want := []string{id + ".json", id + ".json.lock"}; !slices.Equal(names, want) {
		t.Fatalf("jobs/ holds %v, want %v", names, want)
	}
}

// TestDelegationRoundTrips: a job handed to an external system keeps that
// system's handle, which is the only thing that can find the work again after
// every process involved has exited.
func TestDelegationRoundTrips(t *testing.T) {
	s, _ := newTestStore(t)
	id, _ := s.Submit(sampleRecord())
	held, _ := s.Claim(id, "go-worker", time.Minute)

	if _, err := s.Update(id, held.Lease.Epoch, func(r *Record) error {
		r.Delegation = &Delegation{System: "bits", ExternalID: "{6f8c1a2b-...}"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r, _ := s.Load(id)
	if !r.Delegated() || r.Delegation.System != "bits" {
		t.Fatalf("delegation did not survive: %+v", r.Delegation)
	}

	// Half a delegation is worse than none: a system with no handle, or a
	// handle nobody can interpret, is unreachable work.
	if _, err := s.Update(id, held.Lease.Epoch, func(r *Record) error {
		r.Delegation = &Delegation{System: "bits"}
		return nil
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("update = %v, want ErrInvalid for a delegation with no external id", err)
	}
}

func TestDecodeRefusesUnknownSchema(t *testing.T) {
	_, err := Decode([]byte(`{"schema":99,"id":"x","kind":"k","state":"pending","spec":{}}`))
	if !errors.Is(err, ErrUnknownSchema) && !errors.Is(err, ErrInvalid) {
		t.Fatalf("decode = %v, want a refusal", err)
	}
}

// TestRecordIsReadableWhileHeld: a process with no lease, and no intention of
// getting one, can still see how far the job has got. Progress available only
// through a callback is bound to the lifetime of the process that registered
// it — and that is precisely the lifetime that fails here.
func TestRecordIsReadableWhileHeld(t *testing.T) {
	s, _ := newTestStore(t)
	id, _ := s.Submit(sampleRecord())
	held, _ := s.Claim(id, "go-worker", time.Minute)
	s.Update(id, held.Lease.Epoch, func(r *Record) error {
		r.Progress.Done = 250
		r.Progress.Total = 1000
		return nil
	})

	observer, err := NewFileStore(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	r, err := observer.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if r.Progress.Done != 250 || r.Lease.Owner != "go-worker" {
		t.Fatalf("observer saw %+v", r.Progress)
	}
}

func TestSubmitRefusesDuplicateID(t *testing.T) {
	s, _ := newTestStore(t)
	r := sampleRecord()
	r.ID = "fixed-id"
	if _, err := s.Submit(r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(r); err == nil {
		t.Fatal("submitting the same id twice was allowed; it would overwrite a running job")
	}
}

func TestRecordFileIsPlainReadableJSON(t *testing.T) {
	s, _ := newTestStore(t)
	id, _ := s.Submit(sampleRecord())
	b, err := os.ReadFile(filepath.Join(s.Root(), "jobs", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	// The record is the cross-language contract, so it has to be something a
	// human and a Python program can both read without special tooling.
	if !strings.Contains(string(b), "\n  \"id\"") {
		t.Fatalf("record is not indented JSON:\n%s", b)
	}
}

// A finished job must not look like stranded work.
//
// StateTransferred is deliberately not terminal — the requester still has to
// take delivery — so Claimable says yes once the lease lapses. A supervisor
// sweeping for orphans therefore re-ran a job that was already complete and
// verified. On a NAS that meant re-downloading 313 MB every 30 seconds, and it
// would have gone on forever. Found by running it, not by reading it.
func TestTransferredJobIsNotAnOrphan(t *testing.T) {
	s, c := newTestStore(t)
	id, err := s.Submit(sampleRecord())
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.Claim(id, "worker", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(id, rec.Lease.Epoch, func(r *Record) error {
		r.State = StateTransferred
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c.add(time.Hour) // the lease lapses, as it would after a crash

	orphans, err := s.Orphans()
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range orphans {
		if o.ID == id {
			t.Fatal("a transferred job was offered up as an orphan; a supervisor will download it all over again")
		}
	}

	// But it is still claimable by anything that means to take delivery of it —
	// only the rescue sweep should leave it alone.
	if _, err := s.Claim(id, "consumer", time.Second); err != nil {
		t.Fatalf("taking delivery must still be possible: %v", err)
	}
}

// Releasing a DELEGATED job must not demote it to pending.
//
// Delegation deliberately releases the lease straight away — holding it would
// stop anyone else polling or finalising. But Release also turned RUNNING into
// PENDING, so a job that BITS or a NAS was actively downloading looked, to
// every supervisor sweeping for stranded work, exactly like a job nobody had
// started. The second tier would fetch the same bytes all over again while the
// first was still going.
func TestReleasingDelegatedJobKeepsItRunning(t *testing.T) {
	s, _ := newTestStore(t)
	id, err := s.Submit(sampleRecord())
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.Claim(id, "delegator", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(id, rec.Lease.Epoch, func(r *Record) error {
		r.Delegation = &Delegation{System: "nas", ExternalID: "remote-1"}
		r.State = StateRunning
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(id, rec.Lease.Epoch); err != nil {
		t.Fatal(err)
	}

	got, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateRunning {
		t.Fatalf("state is %s; a job running inside another system is not pending", got.State)
	}
	if !got.Delegated() {
		t.Fatal("the delegation handle was lost; nothing can find the work again")
	}

	// An undelegated job still goes back to pending, which is what Release is
	// for in the ordinary case.
	plain, err := s.Submit(sampleRecord())
	if err != nil {
		t.Fatal(err)
	}
	pr, err := s.Claim(plain, "worker", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Release(plain, pr.Lease.Epoch); err != nil {
		t.Fatal(err)
	}
	after, err := s.Load(plain)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != StatePending {
		t.Fatalf("an ordinary released job should be pending, got %s", after.State)
	}
}

// A claim writes the record as it is under the lock, not as the claimant read
// it. The old compare-then-rename compared the epoch and wrote the caller's
// copy, which erased an intent set in between while reporting success.
func TestAClaimKeepsWhatWasWrittenSinceTheCallerRead(t *testing.T) {
	s, _ := newTestStore(t)
	id, _ := s.Submit(sampleRecord())
	seen, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetIntent(id, WantPause, "ui"); err != nil {
		t.Fatal(err)
	}
	held, err := s.ClaimFrom(seen, "sweeper", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if held.Wants() != WantPause {
		t.Fatal("the claim returned a record without the pause set since it was read")
	}
	r, _ := s.Load(id)
	if r.Wants() != WantPause || r.Lease.Owner != "sweeper" {
		t.Fatalf("the claim erased the pause: intent %v, owner %q", r.Intent, r.Lease.Owner)
	}
}

// A claim computed from a record that has since moved must not commit: the
// epoch would go backwards, a zombie holding the earlier number would pass the
// staleness check in Update, and two processes would work one job while every
// command reported success.
func TestAStaleClaimCannotCommitOverANewerOwner(t *testing.T) {
	s, c := newTestStore(t)
	id, _ := s.Submit(sampleRecord())

	// What a straggler read before it was descheduled: the job, unclaimed.
	seen, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}

	// Two claims go through while it is not looking.
	zombie, err := s.Claim(id, "first", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.add(31 * time.Second)
	current, err := s.Claim(id, "second", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if current.Lease.Epoch != 2 {
		t.Fatalf("staging: the record is at epoch %d, want 2", current.Lease.Epoch)
	}

	// The straggler wakes up and finishes the claim it started.
	if _, err := s.ClaimFrom(seen, "straggler", 30*time.Second); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("a claim computed from epoch %d committed over epoch %d: %v",
			seen.Lease.Epoch, current.Lease.Epoch, err)
	}

	after, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Lease.Epoch != 2 || after.Lease.Owner != "second" {
		t.Fatalf("the lease is %q at epoch %d; the epoch went backwards and the "+
			"owner doing the work no longer owns it", after.Lease.Owner, after.Lease.Epoch)
	}

	// The damage that would have followed: the first owner's lease expired and
	// its writes must stay refused. They only stay refused while the epoch on
	// disk is above its own.
	if _, err := s.Update(id, zombie.Lease.Epoch, func(r *Record) error {
		r.Progress.Done = 999
		return nil
	}); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("a zombie owner's write = %v, want ErrStaleEpoch", err)
	}

}

// Every write here is load, edit, replace, and a filesystem does not make those
// one operation. Two that overlapped used to lose one, silently, and the loser
// was usually the thing somebody meant: a renewing owner is a standing writer —
// three times a lease TTL for the whole of a transfer — so it erased pauses and
// sent checkpoints backwards while reporting success to everybody.
func TestAConcurrentWriteIsNotOverwritten(t *testing.T) {
	s, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Submit(sampleRecord())
	if err != nil {
		t.Fatal(err)
	}
	held, err := s.Claim(id, "owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	epoch := held.Lease.Epoch

	stop := make(chan struct{})
	var renewing sync.WaitGroup
	renewing.Add(1)
	go func() {
		defer renewing.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.Renew(id, epoch, time.Minute)
		}
	}()

	const steps = 50
	for n := int64(1); n <= steps; n++ {
		if _, err := s.Update(id, epoch, func(r *Record) error {
			r.Progress.Done = n
			return nil
		}); err != nil {
			t.Fatalf("progress %d was refused: %v", n, err)
		}
	}
	if _, err := s.SetIntent(id, WantPause, "ui"); err != nil {
		t.Fatalf("the pause was refused: %v", err)
	}
	close(stop)
	renewing.Wait()

	r, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if r.Progress.Done != steps {
		t.Fatalf("a renewal put the checkpoint back to %d of %d", r.Progress.Done, steps)
	}
	if r.Wants() != WantPause {
		t.Fatal("a renewal erased the pause")
	}
}
