package job

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The reserved set is read off a real store, not copied from the documentation.
//
// A store that grows a directory and a Reserved that does not is the failure
// this pins: the layer above would then be told a path is free space when it
// names something the store is about to write. So drive a store through the
// operations that create files — submit, claim, update, a claim that has to age
// out a token — and require that every path it left is reserved against some
// other job.
func TestReservedCoversEveryPathAStoreWrites(t *testing.T) {
	root := t.TempDir()
	s, err := NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Submit(Record{Kind: "download", Spec: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Claim(id, "owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(id, r.Lease.Epoch, func(r *Record) error { r.Progress.Done = 1; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.WorkPath(id), []byte("a partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	const stranger = "0000000000000-000000000000"
	var found int
	err = filepath.WalkDir(root, func(p string, _ os.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		found++
		if !Reserved(stranger, rel) {
			t.Errorf("the store wrote %q and Reserved calls it free space", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found < 4 {
		t.Fatalf("the store left %d paths; this test is not exercising it", found)
	}

	// And the one path the store hands OUT rather than keeps: a job's own
	// scratch, which the layer above is told to use.
	own, rerr := filepath.Rel(root, s.WorkPath(id))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if Reserved(id, own) {
		t.Errorf("Reserved(%q, %q) refuses a job its own scratch", id, own)
	}
}

func TestReservedNamesTheLayoutAndNothingElse(t *testing.T) {
	const me = "1757000000000-deadbeef"
	const other = "1757000000001-cafebabe"

	for _, p := range []string{
		"jobs",
		"jobs/x.json",
		"jobs/" + me + ".json",
		"jobs/" + me + ".json.lock",
		"jobs/" + me + ".tmp-123",
		"jobs/" + me + ".json.123.tmp",
		"jobs/deeper/still.json",
		"work",
		"work/" + other,
		"work/" + other + "/part",
		"services.json",

		// The spellings a filesystem would fold into the ones above.
		"Jobs/x.json",
		"JOBS/x.json",
		`jobs\x.json`,
		"jobs./x.json",
		"WORK/" + other,
		"Services.json",
		"models/../jobs/x.json",
		"./jobs/x.json",
	} {
		if !Reserved(me, p) {
			t.Errorf("Reserved(%q, %q) = false, want true", me, p)
		}
	}

	for _, p := range []string{
		"",
		"models/x.gguf",
		"work/" + me,
		"work/" + me + "/part",
		"work/" + strings.ToUpper(me),
		"jobsy/x.json",
		"myjobs/x.json",
		"a/jobs/x.json",
		"a/services.json",
		"services.json.bak",
		"jobs/../models/x.gguf",
		"supervisor.json", // download's, not the job layer's — see download.ReservedSink
	} {
		if Reserved(me, p) {
			t.Errorf("Reserved(%q, %q) = true, want false", me, p)
		}
	}

	// No owner means no job owns anything, so the whole of work/ is the
	// store's. That is what a caller asks before it has been given an id.
	for _, p := range []string{"work/" + me, "work/" + other} {
		if !Reserved("", p) {
			t.Errorf(`Reserved("", %q) = false, want true`, p)
		}
	}
}
