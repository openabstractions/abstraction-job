package job

import (
	"testing"
	"time"
)

func TestWatchSeesWorkThatPredatesTheSubscription(t *testing.T) {
	var s Store = openStore(t)
	id := submitOne(t, s) // exists BEFORE anyone watches

	sub := Watch(s, "test")
	defer sub.Close()

	got := sub.Records()
	if len(got) != 1 || got[0].ID != id {
		t.Fatalf("initial snapshot = %d records, want the one submitted earlier", len(got))
	}
}

func TestWatchFiltersByKind(t *testing.T) {
	var s Store = openStore(t)
	submitOne(t, s)

	var other Record
	other.Kind = "something-else"
	if err := other.SetSpec(map[string]string{"x": "y"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(other); err != nil {
		t.Fatal(err)
	}

	sub := Watch(s, "test")
	defer sub.Close()
	if n := len(sub.Records()); n != 1 {
		t.Fatalf("snapshot = %d records, want 1 — a kind filter that leaks shows a UI somebody else's work", n)
	}
}

func TestWatchPushesAChange(t *testing.T) {
	var s Store = openStore(t)
	sub := Watch(s, "test")
	defer sub.Close()

	id := submitOne(t, s)

	select {
	case snap := <-sub.Changes():
		if len(snap) != 1 || snap[0].ID != id {
			t.Fatalf("pushed snapshot = %v, want the new job", snap)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no change delivered for a newly submitted job")
	}
}

// A lease renewal changes UpdatedAt and nothing a person can see. If that woke
// the UI, a progress bar would flicker on every heartbeat.
func TestWatchIgnoresChangesNobodyCanSee(t *testing.T) {
	var s Store = openStore(t)
	id := submitOne(t, s)
	sub := Watch(s, "test")
	defer sub.Close()

	claimed, err := s.Claim(id, "owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// Drain the change the claim legitimately caused (state and lease owner).
	select {
	case <-sub.Changes():
	case <-time.After(10 * time.Second):
		t.Fatal("claim should have been visible")
	}

	if _, err := s.Renew(id, claimed.Lease.Epoch, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	select {
	case snap := <-sub.Changes():
		t.Fatalf("renewing a lease woke the watcher: %v", snap)
	case <-time.After(2 * time.Second):
	}
}
