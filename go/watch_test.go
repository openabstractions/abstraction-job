package job

import (
	"context"
	"errors"
	"testing"
	"time"

	watch "github.com/openabstractions/abstraction-watch/go"
)

const watchBudget = 200 * time.Millisecond

func nextNotice(t *testing.T, sub Subscription) Notice {
	t.Helper()
	n, err := sub.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWatchSeesWorkThatPredatesTheSubscription(t *testing.T) {
	var s Store = openStore(t)
	id := submitOne(t, s)

	sub := Watch(s, "test")
	defer sub.Close()

	got := sub.Records()
	if len(got) != 1 || got[0].ID != id {
		t.Fatalf("initial snapshot = %d records, want the one submitted earlier", len(got))
	}
	if n := nextNotice(t, sub); len(n.Records) != 1 || n.Quiet {
		t.Fatalf("first notice = %+v, want the present", n)
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

func TestWatchReportsQuietOnlyWhenNothingVisibleMoved(t *testing.T) {
	var s Store = openStore(t)
	id := submitOne(t, s)
	sub := WatchQuiet(s, "test", watchBudget)
	defer sub.Close()
	nextNotice(t, sub)

	claimed, err := s.Claim(id, "owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if n := nextNotice(t, sub); n.Quiet || n.Records[0].State != StateRunning {
		t.Fatalf("after a claim: %+v, want the change", n)
	}

	if _, err := s.Renew(id, claimed.Lease.Epoch, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	if n := nextNotice(t, sub); !n.Quiet || n.Silence < watchBudget {
		t.Fatalf("after a renewal: %+v, want quiet — a renewal is invisible", n)
	}
}

func TestWatchClosedEndsTheStream(t *testing.T) {
	var s Store = openStore(t)
	sub := Watch(s, "test")
	nextNotice(t, sub)
	go sub.Close()
	if _, err := sub.Next(context.Background()); !errors.Is(err, watch.ErrClosed) {
		t.Fatalf("err = %v after Close, want ErrClosed", err)
	}
}
