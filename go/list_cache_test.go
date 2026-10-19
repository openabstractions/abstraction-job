package job

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// countRecordReads counts Load's record file reads for the rest of the test.
func countRecordReads(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	prior := readRecord
	readRecord = func(path string) ([]byte, error) {
		n.Add(1)
		return prior(path)
	}
	t.Cleanup(func() { readRecord = prior })
	return &n
}

// settle moves every record's write time a minute back, as on a store whose
// jobs finished earlier.
func settle(t *testing.T, s *FileStore) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(s.Root(), "jobs"))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	for _, e := range entries {
		if err := os.Chtimes(filepath.Join(s.Root(), "jobs", e.Name()), old, old); err != nil {
			t.Fatal(err)
		}
	}
}

// An idle store is listed by the job watcher every poll and by executor
// sweeps through Orphans. After the first read, those Lists answer from the
// records already decoded and open no record file; a write is read again.
func TestIdleStoreListsWithoutRereadingRecords(t *testing.T) {
	s, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const records = 200
	ids := make([]string, 0, records)
	for i := range records {
		id, err := s.Submit(Record{Kind: "idle", Spec: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	settle(t, s)
	reads := countRecordReads(t)

	// The production watcher, polling every 20 ms.
	sub := WatchQuiet(s, "idle", 20*time.Millisecond)
	defer sub.Close()
	if got := len(sub.Records()); got != records {
		t.Fatalf("watcher saw %d records, want %d", got, records)
	}
	if got := reads.Load(); got != records {
		t.Fatalf("first list read %d records, want %d", got, records)
	}

	reads.Store(0)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	polls := 0
	// A poll slower than the budget answers quiet at once, so the loop
	// keeps its own deadline.
	for ctx.Err() == nil {
		if _, err := sub.Next(ctx); err != nil {
			break
		}
		polls++
		if _, err := s.Orphans(); err != nil {
			t.Fatal(err)
		}
	}
	if polls < 3 {
		t.Fatalf("watcher answered %d times in 300 ms", polls)
	}
	if got := reads.Load(); got != 0 {
		t.Fatalf("idle polls read %d record files, want 0", got)
	}

	// A caller's edit to a listed record stays its own.
	all, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	all[0].State = StateFailed
	all[0].Spec[0] = 'x'
	again, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if again[0].State != StatePending || again[0].Spec[0] != '{' {
		t.Fatalf("cached record changed by a caller: %s %s", again[0].State, again[0].Spec)
	}

	// A write is read on the next List, and only that record.
	reads.Store(0)
	if _, err := s.SetIntent(ids[7], WantPause, "test"); err != nil {
		t.Fatal(err)
	}
	reads.Store(0)
	again, err = s.List()
	if err != nil {
		t.Fatal(err)
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("list after one write read %d record files, want 1", got)
	}
	if again[7].ID != ids[7] || again[7].Wants() != WantPause {
		t.Fatalf("list missed the write: %s wants %s", again[7].ID, again[7].Wants())
	}

	// A record that disappears leaves the listing.
	if err := os.Remove(filepath.Join(s.Root(), "jobs", ids[3]+".json")); err != nil {
		t.Fatal(err)
	}
	again, err = s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != records-1 {
		t.Fatalf("list after a removal has %d records, want %d", len(again), records-1)
	}
}

// A record rewritten with the same size and an old write time copied onto it
// is still a different file once its write time differs from the cached one.
func TestListRereadsRecordWithChangedWriteTime(t *testing.T) {
	s, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Submit(Record{Kind: "idle", Spec: json.RawMessage(`{"n":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	settle(t, s)
	if _, err := s.List(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Root(), "jobs", id+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	r.Spec = json.RawMessage(`{"n":2}`)
	nb, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if len(nb) != len(b) {
		t.Fatalf("test needs an equal-size rewrite: %d != %d", len(nb), len(b))
	}
	if err := os.WriteFile(path, nb, 0o644); err != nil {
		t.Fatal(err)
	}
	older := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(path, older, older); err != nil {
		t.Fatal(err)
	}
	all, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	var spec struct{ N int }
	if err := json.Unmarshal(all[0].Spec, &spec); err != nil || spec.N != 2 {
		t.Fatalf("list kept the old record: %s", all[0].Spec)
	}
}

// clone copies every field and shares no pointer, slice or map with the
// original. A new Record field fails the sample check until both the sample
// and clone carry it.
func TestRecordCloneIsDeep(t *testing.T) {
	r := &Record{
		Content:    []string{"a"},
		Critical:   []string{"b"},
		ID:         "id",
		Kind:       "kind",
		Envelope:   &Envelope{Schema: "x/y@1", Actions: []string{"cancel"}},
		State:      StateRunning,
		Spec:       json.RawMessage(`{}`),
		Checkpoint: json.RawMessage(`{}`),
		Progress:   Progress{Done: 1, Total: 2, UpdatedAt: At(time.Now()), Step: &Step{Name: "s", Ordinal: 1}},
		Lease:      Lease{Owner: "o", Epoch: 1, ExpiresAt: At(time.Now()), Recall: &Recall{Reason: "r"}},
		Delegation: &Delegation{System: "s", ExternalID: "e"},
		Requires:   []string{"c"},
		Error:      "e",
		Intent:     &Intent{Want: WantPause},
		Extensions: map[string]json.RawMessage{"k": json.RawMessage(`1`)},
		CreatedAt:  At(time.Now()),
		UpdatedAt:  At(time.Now()),
	}
	v := reflect.ValueOf(r).Elem()
	for i := range v.NumField() {
		if v.Field(i).IsZero() {
			t.Fatalf("sample leaves Record.%s empty", v.Type().Field(i).Name)
		}
	}
	c := r.clone()
	if !reflect.DeepEqual(r, c) {
		t.Fatalf("clone differs:\n%+v\n%+v", r, c)
	}
	assertNoAlias(t, "Record", reflect.ValueOf(r).Elem(), reflect.ValueOf(c).Elem())
}

func assertNoAlias(t *testing.T, path string, a, b reflect.Value) {
	t.Helper()
	switch a.Kind() {
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return
		}
		if a.Pointer() == b.Pointer() {
			t.Fatalf("%s is shared", path)
		}
		assertNoAlias(t, path, a.Elem(), b.Elem())
	case reflect.Slice:
		if a.Len() > 0 && a.Pointer() == b.Pointer() {
			t.Fatalf("%s is shared", path)
		}
		for i := range a.Len() {
			assertNoAlias(t, fmt.Sprintf("%s[%d]", path, i), a.Index(i), b.Index(i))
		}
	case reflect.Map:
		if a.Len() > 0 && a.Pointer() == b.Pointer() {
			t.Fatalf("%s is shared", path)
		}
		for _, k := range a.MapKeys() {
			assertNoAlias(t, fmt.Sprintf("%s[%v]", path, k), a.MapIndex(k), b.MapIndex(k))
		}
	case reflect.Struct:
		for i := range a.NumField() {
			assertNoAlias(t, path+"."+a.Type().Field(i).Name, a.Field(i), b.Field(i))
		}
	}
}

// A record List read inside the settle window and then rewritten with the
// same size in the same timestamp tick keeps one directory entry for both
// writes. Once that write time settles, List answers the newer bytes.
func TestListRereadsSameTickRewriteAfterSettle(t *testing.T) {
	s, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Submit(Record{Kind: "idle", Spec: json.RawMessage(`{"n":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	clock := base
	prior := listNow
	listNow = func() time.Time { return clock }
	t.Cleanup(func() { listNow = prior })
	path := filepath.Join(s.Root(), "jobs", id+".json")
	tick := base.Add(-time.Second)
	if err := os.Chtimes(path, tick, tick); err != nil {
		t.Fatal(err)
	}
	// The watcher's List inside the window, between the two writes.
	if _, err := s.List(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	r.Spec = json.RawMessage(`{"n":2}`)
	nb, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if len(nb) != len(b) {
		t.Fatalf("test needs an equal-size rewrite: %d != %d", len(nb), len(b))
	}
	if err := os.WriteFile(path, nb, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, tick, tick); err != nil {
		t.Fatal(err)
	}
	clock = base.Add(10 * time.Second)
	all, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	var spec struct{ N int }
	if err := json.Unmarshal(all[0].Spec, &spec); err != nil || spec.N != 2 {
		t.Fatalf("list answered the bytes it read before the same-tick rewrite: %s", all[0].Spec)
	}
}
