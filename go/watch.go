package job

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	watch "github.com/openabstractions/abstraction-watch/go"
)

// Notice is the present of one kind's jobs, and whether it has stopped moving.
type Notice struct {
	Records []*Record
	Quiet   bool
	Silence time.Duration
}

// Subscription is a live view of the jobs of one kind. Records is what was true
// at the last read; the processes doing this work are not this one, so re-read
// rather than cache across a decision.
//
// Next and Changes are two spellings of one stream: drive a subscription with
// one of them, not both.
type Subscription interface {
	Records() []*Record
	Next(ctx context.Context) (Notice, error)
	Changes() <-chan []*Record
	Close() error
}

// Watchable is an optional capability: a binding that can say when something
// changed instead of being asked. The file binding cannot — filesystem
// notification is per-OS, misses events under load, and does not exist over
// the SMB mount a NAS store lives on — so Watch falls back to asking, and the
// fallback is visible rather than hidden: only the latency differs.
type Watchable interface {
	Subscribe(kind string) (Subscription, error)
}

// pollEvery is fast enough that a person clicking pause sees it, slow enough
// that a store on a network share is not being hammered.
const pollEvery = 750 * time.Millisecond

// Watch is WatchQuiet without a budget: it reports change and never silence.
func Watch(s Store, kind string) Subscription { return WatchQuiet(s, kind, 0) }

// WatchQuiet reports the jobs of one kind, and reports quiet once nothing
// visible has changed for budget.
//
// kind is required. A store holds every kind of work on the machine, and
// selecting on Kind, which this layer owns, is what keeps Spec, which it does
// not, opaque.
func WatchQuiet(s Store, kind string, budget time.Duration) Subscription {
	if w, ok := s.(Watchable); ok {
		if sub, err := w.Subscribe(kind); err == nil {
			return sub
		}
	}
	every := pollEvery
	if budget > 0 && budget < every {
		every = budget
	}
	read := func() ([]*Record, string, error) {
		all, err := s.List()
		if err != nil {
			return nil, "", err
		}
		mine := make([]*Record, 0, len(all))
		for _, r := range all {
			if r.Kind == kind {
				mine = append(mine, r)
			}
		}
		return mine, fingerprint(mine), nil
	}
	return &watcher{sub: watch.Poll(read, every, budget), ch: make(chan []*Record, 1)}
}

type watcher struct {
	sub  *watch.Subscription[[]*Record]
	ch   chan []*Record
	once sync.Once
}

func (w *watcher) Records() []*Record { return snapshotOf(w.sub.Current()) }

func (w *watcher) Next(ctx context.Context) (Notice, error) {
	n, err := w.sub.Next(ctx)
	return Notice{Records: snapshotOf(n.Now), Quiet: n.Quiet, Silence: n.Silence}, err
}

func (w *watcher) Changes() <-chan []*Record {
	w.once.Do(func() { go w.forward() })
	return w.ch
}

func (w *watcher) forward() {
	for {
		n, err := w.sub.Next(context.Background())
		if err != nil {
			return
		}
		if n.Quiet {
			continue
		}
		select {
		case <-w.ch:
		default:
		}
		w.ch <- snapshotOf(n.Now)
	}
}

func (w *watcher) Close() error { return w.sub.Close() }

func snapshotOf(rs []*Record) []*Record {
	out := make([]*Record, len(rs))
	copy(out, rs)
	return out
}

// fingerprint is what "changed" means: identity, state, progress, owner and
// error. Not UpdatedAt — a lease renewal moves it and nothing a person can
// see, and a view that redraws on every heartbeat flickers.
func fingerprint(rs []*Record) string {
	var b strings.Builder
	for _, r := range rs {
		b.WriteString(r.ID)
		b.WriteByte('|')
		b.WriteString(string(r.State))
		b.WriteByte('|')
		b.WriteString(strconv.FormatInt(r.Progress.Done, 10))
		b.WriteByte('/')
		b.WriteString(strconv.FormatInt(r.Progress.Total, 10))
		b.WriteByte('|')
		b.WriteString(r.Lease.Owner)
		b.WriteByte('|')
		b.WriteString(r.Error)
		b.WriteByte('\n')
	}
	return b.String()
}
