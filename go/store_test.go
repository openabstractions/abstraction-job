package job

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// TestEpochTokensDoNotBlock: a token left by a process that was killed must not
// stop the next owner. This is why the epoch is in the filename rather than
// there being one lockfile to break.
func TestEpochTokensDoNotBlock(t *testing.T) {
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
	for i := 1; i <= 3; i++ {
		if _, err := os.Stat(s.epochPath(id, int64(i))); err != nil {
			t.Fatalf("epoch token %d missing: %v", i, err)
		}
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
