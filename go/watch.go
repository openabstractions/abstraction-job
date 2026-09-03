package job

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// Subscription is a live view of the jobs of one kind: what they are now, and a
// signal when that changes.
//
// # Why this exists at all
//
// Every design decision before it was made against a command line that prints a
// line and exits. An application with a window needs the other shape — a
// collection it can bind to, that already contains the work which was in flight
// before this process started, and that keeps being right afterwards. Without
// one, every application writes its own polling loop, which is the private
// reimplementation this project exists to stop.
//
// # It is a view, and views go stale
//
// Records returns what was true at the last read. The processes doing this work
// are not this one; any of them may have died since. Re-read, never cache across
// a decision. That is the same discipline the record imposes, and it is why the
// snapshot is handed out as a value rather than as something to hold.
type Subscription interface {
	// Records is the current snapshot, oldest first.
	Records() []*Record
	// Changes delivers a new snapshot when one differs from the last.
	//
	// Coalescing and lossy on purpose: a slow consumer gets the latest state,
	// not a backlog of every state it missed. A UI wants to know what is true
	// now — nobody wants to redraw a progress bar through history — and a
	// channel that blocks would make the watcher a way to wedge the writer.
	Changes() <-chan []*Record
	// Close stops the subscription and releases whatever is behind it.
	Close() error
}

// Watchable is an OPTIONAL capability: a binding that can say when something
// changed, instead of being asked.
//
// A service binding knows the moment a record moves and can push. The file
// binding does not, and there is no portable way to make it — filesystem
// notification is per-OS, misses events under load, and does not work at all
// over the SMB mount that a NAS store lives on. So Watch below falls back to
// asking, and the fallback is honest rather than hidden: an application gets a
// live collection either way, and only the latency differs.
type Watchable interface {
	Subscribe(kind string) (Subscription, error)
}

// pollEvery is fast enough that a person clicking pause sees it, slow enough
// that a store on a network share is not being hammered. It only applies to
// bindings that cannot push.
const pollEvery = 750 * time.Millisecond

// Watch returns a live view of the jobs of one kind, using the binding's own
// notification when it has one and polling when it does not.
//
// kind is required and is not a nicety. A store holds every kind of work on the
// machine, and an application that renders "all jobs" will one day render
// somebody else's. Filtering here is also what keeps the opaque spec opaque: the
// job layer selects on Kind, which it owns, and never looks inside Spec, which
// it does not.
func Watch(s Store, kind string) Subscription {
	if w, ok := s.(Watchable); ok {
		if sub, err := w.Subscribe(kind); err == nil {
			return sub
		}
		// A binding that advertises Subscribe and then fails is not a reason to
		// give the application nothing. Fall through and ask instead.
	}
	p := &poller{store: s, kind: kind, ch: make(chan []*Record, 1), done: make(chan struct{})}
	p.refresh()
	go p.run()
	return p
}

type poller struct {
	store Store
	kind  string

	mu    sync.RWMutex
	recs  []*Record
	stamp string

	ch   chan []*Record
	done chan struct{}
	once sync.Once
}

func (p *poller) Records() []*Record {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Record, len(p.recs))
	copy(out, p.recs)
	return out
}

func (p *poller) Changes() <-chan []*Record { return p.ch }

func (p *poller) Close() error {
	p.once.Do(func() { close(p.done) })
	return nil
}

func (p *poller) run() {
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-t.C:
			if p.refresh() {
				select {
				case p.ch <- p.Records():
				default:
					// Consumer is behind. Drop this one: the snapshot in the
					// channel is already newer than what it has, and it will
					// take the next one.
				}
			}
		}
	}
}

// refresh re-reads and reports whether anything the caller could see changed.
func (p *poller) refresh() bool {
	all, err := p.store.List()
	if err != nil {
		// A store that cannot be read right now is not the same as an empty
		// one, and reporting empty would make a UI erase a list of live work.
		// Keep the last good snapshot and try again.
		return false
	}
	mine := make([]*Record, 0, len(all))
	for _, r := range all {
		if r.Kind == p.kind {
			mine = append(mine, r)
		}
	}
	stamp := fingerprint(mine)

	p.mu.Lock()
	defer p.mu.Unlock()
	if stamp == p.stamp {
		return false
	}
	p.recs, p.stamp = mine, stamp
	return true
}

// fingerprint is what "changed" means here: identity, state, progress and the
// lease. Deliberately NOT the whole record — UpdatedAt alone would report a
// change every time an owner renewed a lease with nothing to say, and a UI that
// redraws on a heartbeat is a UI that flickers.
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
