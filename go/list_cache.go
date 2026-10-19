package job

import (
	"encoding/json"
	"maps"
	"os"
	"slices"
	"sync"
	"time"
)

// listSettle is how long a record file must have kept its write time before
// List answers it from memory. A write inside the window can leave size and
// write time unchanged on a file system with coarse timestamps (FAT keeps two
// seconds), so a record that recent is read again on every List.
const listSettle = 5 * time.Second

// listNow is List's clock for the settle window, compared with file write
// times; tests move it.
var listNow = time.Now

// listCache keeps the last decoded copy of each record List read, keyed by
// the size and write time its directory entry showed before the read. Every
// write replaces the record file through cas, which gives it a new write
// time, so an unchanged entry names the bytes already decoded. Watchers and
// sweeps list the store every second; with this cache an idle store costs
// one directory listing per List instead of one open, read and decode per
// record.
type listCache struct {
	mu   sync.Mutex
	rows map[string]listRow
}

type listRow struct {
	size    int64
	modTime time.Time
	record  *Record // never handed out; List returns clones
}

// cached answers a clone of the record whose directory entry still shows
// size and modTime, when that write time settled before now.
func (c *listCache) cached(id string, info os.FileInfo, now time.Time) *Record {
	if info == nil || now.Sub(info.ModTime()) < listSettle {
		return nil
	}
	c.mu.Lock()
	row, ok := c.rows[id]
	c.mu.Unlock()
	if !ok || row.size != info.Size() || !row.modTime.Equal(info.ModTime()) {
		return nil
	}
	return row.record.clone()
}

// remember keeps a clone of r under the directory entry seen before it was
// read, when that write time had settled at now, taken before the listing.
// A later write then carries a write time past the settle window and a
// different entry, and the next List reads again. A record still inside the
// window stays uncached: a same-size rewrite in the same timestamp tick
// would otherwise keep this copy under the entry of the newer bytes.
func (c *listCache) remember(id string, info os.FileInfo, r *Record, now time.Time) {
	if info == nil || now.Sub(info.ModTime()) < listSettle {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rows == nil {
		c.rows = map[string]listRow{}
	}
	c.rows[id] = listRow{size: info.Size(), modTime: info.ModTime(), record: r.clone()}
}

// keep drops the rows of records no longer listed.
func (c *listCache) keep(listed map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.rows {
		if !listed[id] {
			delete(c.rows, id)
		}
	}
}

// clone is a deep copy: a caller may edit the record List returned without
// touching the cached one or another caller's copy.
func (r *Record) clone() *Record {
	if r == nil {
		return nil
	}
	c := *r
	c.Content = slices.Clone(r.Content)
	c.Critical = slices.Clone(r.Critical)
	c.Requires = slices.Clone(r.Requires)
	c.Spec = cloneRaw(r.Spec)
	c.Checkpoint = cloneRaw(r.Checkpoint)
	if r.Envelope != nil {
		e := *r.Envelope
		e.Actions = slices.Clone(r.Envelope.Actions)
		c.Envelope = &e
	}
	if r.Progress.Step != nil {
		s := *r.Progress.Step
		c.Progress.Step = &s
	}
	if r.Lease.Recall != nil {
		v := *r.Lease.Recall
		c.Lease.Recall = &v
	}
	if r.Delegation != nil {
		d := *r.Delegation
		c.Delegation = &d
	}
	if r.Intent != nil {
		i := *r.Intent
		c.Intent = &i
	}
	if r.Extensions != nil {
		c.Extensions = maps.Clone(r.Extensions)
		for k, v := range c.Extensions {
			c.Extensions[k] = cloneRaw(v)
		}
	}
	return &c
}

func cloneRaw(b json.RawMessage) json.RawMessage {
	if b == nil {
		return nil
	}
	return slices.Clone(b)
}
