package job

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A record id names one file under jobs/. An id that climbs out of it, or
// names another directory, is refused before any path is built from it.
func TestFileStoreRefusesRecordIDsThatLeaveJobs(t *testing.T) {
	s, _ := newTestStore(t)
	// A record outside jobs/ that a traversal id would reach.
	outside := filepath.Join(s.Root(), "outside.json")
	if err := os.WriteFile(outside, []byte(`{"id":"outside"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{
		"../outside",
		`..\outside`,
		"..",
		"sub/record",
		`sub\record`,
		"/abs",
		"", // Submit gives an empty id a new one; every other call refuses it.
	} {
		if _, err := s.Submit(Record{ID: id, Kind: "test", Spec: []byte(`{}`)}); id != "" && !errors.Is(err, ErrInvalid) {
			t.Errorf("Submit(%q) = %v, want ErrInvalid", id, err)
		}
		if _, err := s.Load(id); !errors.Is(err, ErrInvalid) {
			t.Errorf("Load(%q) = %v, want ErrInvalid", id, err)
		}
		if _, err := s.Claim(id, "owner", time.Minute); err == nil {
			t.Errorf("Claim(%q) succeeded", id)
		}
		if _, err := s.Update(id, 1, func(*Record) error { return nil }); !errors.Is(err, ErrInvalid) {
			t.Errorf("Update(%q) = %v, want ErrInvalid", id, err)
		}
	}
	if b, err := os.ReadFile(outside); err != nil || string(b) != `{"id":"outside"}` {
		t.Fatalf("the file outside jobs/ changed: %q, %v", b, err)
	}
	all, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("the store lists %d records, want only the one Submit named for the empty id", len(all))
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "jobs", "sub")); err == nil {
		t.Fatal("a refused id created a directory in the store")
	}

	// An ordinary id still works.
	id, err := s.Submit(Record{ID: "dest-0123abcd-2", Kind: "test", Spec: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(id); err != nil {
		t.Fatal(err)
	}
}
