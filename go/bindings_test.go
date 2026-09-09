package job

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The test this project has been describing for months and could not run.
//
// One body of assertions, executed against three bindings: files in a directory,
// a service over a socket, and a map in memory. Not one line of it knows which.
// If any of it had to change to accommodate one of them, the abstraction was not
// one — and the README said exactly that, in Honest state, as something not yet
// demonstrated.
//
// The memory binding is the one that answers the fair objection to the other
// two: a socket in front of a FileStore is a transport swap, not a second
// implementation. MemoryStore shares no code with FileStore — the lease, the epoch,
// the exclusivity, the stale-write refusal and the rule that a paused job is not
// an orphan are all written again. If the semantics were really FileStore all
// along, this is where it shows.
func TestTheSameAssertionsOnEveryBinding(t *testing.T) {
	for _, b := range bindings(t) {
		t.Run(b.name, func(t *testing.T) { storeContract(t, b.store) })
	}
}

type binding struct {
	name  string
	store Store
}

func bindings(t *testing.T) []binding {
	t.Helper()

	// Binding one: a directory. Needs nothing running.
	fileStore, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// Binding two: a service. The file store behind it is an implementation
	// detail the client cannot see — it could be anything implementing Store.
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

	return []binding{
		{"files", fileStore},
		{"service", NewRemoteStore(ln.Addr().Network(), ln.Addr().String())},
		// Shares no code with the other two. If the semantics were really
		// FileStore all along, this is where that shows.
		{"memory", NewMemoryStore()},
	}
}

// listen prefers a unix socket, because that is what a local service binding
// would really use, and falls back to loopback TCP where one is not available.
func listen(t *testing.T) (net.Listener, error) {
	sock := filepath.Join(t.TempDir(), "s")
	if ln, err := net.Listen("unix", sock); err == nil {
		t.Cleanup(func() { os.Remove(sock) })
		return ln, nil
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

// storeContract is every promise Store makes, written once.
func storeContract(t *testing.T, s Store) {
	t.Helper()

	var r Record
	r.Kind = "test"
	if err := r.SetSpec(map[string]any{"anything": []int{1, 2, 3}}); err != nil {
		t.Fatal(err)
	}
	id, err := s.Submit(r)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Readable by anyone, holding no lease. That is what makes work observable
	// from outside.
	got, err := s.Load(id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Kind != "test" {
		t.Fatalf("kind %q", got.Kind)
	}
	// The spec is opaque and must survive untouched, including a shape this
	// package has no type for.
	var spec struct {
		Anything []int `json:"anything"`
	}
	if err := got.DecodeSpec(&spec); err != nil || len(spec.Anything) != 3 {
		t.Fatalf("the opaque spec did not survive: %v %+v", err, spec)
	}

	// Unclaimed work is available.
	orphans, err := s.Orphans()
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 {
		t.Fatalf("%d orphans, want 1", len(orphans))
	}

	// A claim is exclusive, and the epoch rises.
	claimed, err := s.Claim(id, "first", time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.Lease.Epoch != 1 {
		t.Fatalf("epoch %d, want 1", claimed.Lease.Epoch)
	}
	if _, err := s.Claim(id, "second", time.Minute); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("a second claimant got %v, want ErrLeaseHeld", err)
	}
	// ...and a held job is not available to sweeps.
	if orphans, _ := s.Orphans(); len(orphans) != 0 {
		t.Fatalf("a held job is being offered as an orphan")
	}

	// A write must present the epoch it holds.
	if _, err := s.Update(id, claimed.Lease.Epoch-1, func(*Record) error { return nil }); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("a stale epoch was accepted: %v", err)
	}
	updated, err := s.Update(id, claimed.Lease.Epoch, func(rec *Record) error {
		rec.Progress.Done = 400
		return rec.SetCheckpoint(map[string]int64{"verified_prefix": 400})
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Progress.Done != 400 {
		t.Fatalf("progress %d, want 400", updated.Progress.Done)
	}

	// Intent needs no lease, and that exemption is the feature.
	if _, err := s.SetIntent(id, WantPause, "a-bystander"); err != nil {
		t.Fatalf("set intent: %v", err)
	}
	paused, _ := s.Load(id)
	if paused.Wants() != WantPause || !paused.Paused() {
		t.Fatalf("wants %q paused %v", paused.Wants(), paused.Paused())
	}
	if paused.Intent == nil || paused.Intent.By != "a-bystander" {
		t.Fatal("who asked is part of the record")
	}

	// Releasing hands it back; a paused job still must not be swept up.
	if err := s.Release(id, claimed.Lease.Epoch); err != nil {
		t.Fatalf("release: %v", err)
	}
	if orphans, _ := s.Orphans(); len(orphans) != 0 {
		t.Fatal("a paused job was offered as an orphan; a sweep would resume it")
	}

	// Resumed, it is ordinary work again, and a successor inherits what was
	// proven rather than starting over.
	if _, err := s.SetIntent(id, WantRun, "a-bystander"); err != nil {
		t.Fatal(err)
	}
	next, err := s.Claim(id, "successor", time.Minute)
	if err != nil {
		t.Fatalf("successor claim: %v", err)
	}
	if next.Lease.Epoch != 2 {
		t.Fatalf("epoch %d after re-claim, want 2", next.Lease.Epoch)
	}
	var cp struct {
		VerifiedPrefix int64 `json:"verified_prefix"`
	}
	if err := next.DecodeCheckpoint(&cp); err != nil || cp.VerifiedPrefix != 400 {
		t.Fatalf("successor inherited %+v, want 400 proven", cp)
	}

	// Terminal is terminal. The write that MAKES it terminal is an ordinary
	// update onto a record that is not yet terminal, and it must land; every
	// write after it is refused, including the caller's own release. Update was
	// the one operation none of the three implementations refused, so a lease
	// holder could write progress onto a finished record and Release could walk
	// it back to pending, where the next sweep offered it as an orphan.
	if _, err := s.Update(id, next.Lease.Epoch, func(rec *Record) error {
		rec.State = StateComplete
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(id, "too-late", time.Minute); !errors.Is(err, ErrTerminal) {
		t.Fatalf("a finished job was claimable: %v", err)
	}
	if _, err := s.SetIntent(id, WantCancel, "too-late"); !errors.Is(err, ErrTerminal) {
		t.Fatalf("a finished job accepted an intent: %v", err)
	}
	if _, err := s.Update(id, next.Lease.Epoch, func(rec *Record) error {
		rec.Progress.Done = 999
		return nil
	}); !errors.Is(err, ErrTerminal) {
		t.Fatalf("a finished job accepted a write: %v", err)
	}
	if err := s.Release(id, next.Lease.Epoch); !errors.Is(err, ErrTerminal) {
		t.Fatalf("a finished job accepted a release: %v", err)
	}
	done, _ := s.Load(id)
	if done.State != StateComplete || done.Progress.Done != 400 {
		t.Fatalf("a finished record was written over: %s done=%d", done.State, done.Progress.Done)
	}
	if orphans, _ := s.Orphans(); len(orphans) != 0 {
		t.Fatal("a finished job was offered as an orphan")
	}

	// Not found is not found.
	if _, err := s.Load("no-such-job"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing job gave %v, want ErrNotFound", err)
	}
}

// A caller must be able to discover that a binding has no local area, rather
// than assume one. This is what Scratch being optional buys, and it is
// unfalsifiable while every store is a directory.
func TestOnlyTheFileBindingOffersALocalArea(t *testing.T) {
	bs := bindings(t)
	for _, b := range bs {
		_, isLocal := b.store.(Scratch)
		switch b.name {
		case "files":
			if !isLocal {
				t.Fatal("the file binding should offer Scratch")
			}
		case "service", "memory":
			if isLocal {
				t.Fatalf("the %s binding must not claim to have a local directory", b.name)
			}
		}
	}
}

// Claimable is a pure predicate, so every binding must answer it identically
// for the same record. It did not: two bindings folded "somebody asked this to
// stop" into it and the others did not, so an application got a different
// answer about the same job depending on what was underneath — which is the
// one property this package exists to provide.
//
// Pausing is an INTENT and Claimable reports observed state. Intent is the one
// field writable without a lease, so folding it in makes the predicate change
// meaning under a writer that holds nothing. The sweep is where paused belongs,
// and Orphans says so in every binding.
func TestEveryBindingAnswersClaimableTheSameWay(t *testing.T) {
	type probe struct {
		name    string
		prepare func(*Record)
	}
	probes := []probe{
		{"pending", func(r *Record) {}},
		{"paused", func(r *Record) {
			r.Intent = &Intent{Want: WantPause, By: "a test"}
		}},
		{"cancel requested", func(r *Record) {
			r.Intent = &Intent{Want: WantCancel, By: "a test"}
		}},
		{"terminal", func(r *Record) { r.State = StateComplete }},
	}

	for _, p := range probes {
		t.Run(p.name, func(t *testing.T) {
			var first *bool
			var firstName string
			for _, b := range bindings(t) {
				r := &Record{ID: "probe", Kind: "test", State: StatePending}
				p.prepare(r)
				got := b.store.Claimable(r)
				if first == nil {
					first, firstName = &got, b.name
					continue
				}
				if got != *first {
					t.Fatalf("%s says Claimable=%v and %s says %v for the same record",
						firstName, *first, b.name, got)
				}
			}
		})
	}
}

// And the safety property the predicate must not be relied upon for: whatever
// Claimable says, no binding may offer a paused job up as an orphan.
func TestNoBindingSweepsUpAPausedJob(t *testing.T) {
	for _, b := range bindings(t) {
		t.Run(b.name, func(t *testing.T) {
			id, err := b.store.Submit(Record{Kind: "test", State: StatePending, Spec: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := b.store.SetIntent(id, WantPause, "a test"); err != nil {
				t.Fatal(err)
			}
			orphans, err := b.store.Orphans()
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range orphans {
				if o.ID == id {
					t.Fatal("a paused job was offered as an orphan; a sweep would " +
						"restart it seconds after somebody pressed pause")
				}
			}
		})
	}
}

// Found by accident: a record with no spec was refused by the file and memory
// bindings and accepted by the service one, so the same submission either
// succeeded or failed depending on what was underneath. A binding that accepts
// what the others reject lets a record into a shared store that the next
// reader cannot load.
func TestEveryBindingRefusesTheSameInvalidRecord(t *testing.T) {
	for _, bad := range []struct {
		name string
		rec  Record
	}{
		{"no spec", Record{Kind: "test", State: StatePending}},
		{"no kind", Record{State: StatePending, Spec: []byte(`{}`)}},
		{"spec is not json", Record{Kind: "test", State: StatePending, Spec: []byte(`not json`)}},
	} {
		t.Run(bad.name, func(t *testing.T) {
			var firstErr bool
			var firstName string
			for _, b := range bindings(t) {
				_, err := b.store.Submit(bad.rec)
				got := err != nil
				if firstName == "" {
					firstErr, firstName = got, b.name
					continue
				}
				if got != firstErr {
					t.Fatalf("%s refused=%v but %s refused=%v for the same record",
						firstName, firstErr, b.name, got)
				}
			}
		})
	}
}
