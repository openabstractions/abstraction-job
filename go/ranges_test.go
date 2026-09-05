package job

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixturePath is the record every implementation must produce byte for byte.
// One file, three languages: Go, Python and C++ each build the same record from
// the same ranges and compare. A conformance test that compares bytes is the
// only thing that has ever caught these three disagreeing.
func fixturePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "testdata", "ranges-record.json")
}

// The ranges the fixture is built from: out of order, and with two adjacent
// pairs that must merge. A parallel fetcher's parts finish in whatever order
// they finish, and no caller should have to sort before recording.
func fixtureRanges() []Range {
	return []Range{
		{Start: 20971520, End: 23068672},
		{Start: 8388608, End: 10485760},
		{Start: 0, End: 2097152},
		{Start: 10485760, End: 12582912},
		{Start: 2097152, End: 4194304},
	}
}

func fixtureRecord(t *testing.T) *Record {
	t.Helper()
	at := func(s string) Timestamp {
		parsed, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return At(parsed)
	}
	var r Record
	r.ID = "1787202430967-a752f9a9c2c77b123ffd"
	r.Kind = "download"
	r.State = StateRunning
	if err := r.SetSpec(map[string]any{"artifact": map[string]any{"bytes": 23068672}}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetCheckpointRanges(fixtureRanges()); err != nil {
		t.Fatal(err)
	}
	r.Progress.Done = 10485760
	r.Progress.Total = 23068672
	r.Progress.UpdatedAt = at("2026-08-20T05:07:14.951609Z")
	r.Lease.Owner = "go-worker"
	r.Lease.Epoch = 2
	r.Lease.ExpiresAt = at("2026-08-20T05:08:14.635068Z")
	r.CreatedAt = at("2026-08-20T05:07:10.967343Z")
	r.UpdatedAt = at("2026-08-20T05:07:15.134811Z")
	return &r
}

// The point of a canonical form, tested the only way it can be tested: against
// the other two implementations' bytes.
func TestARangeCheckpointEncodesToTheAgreedBytes(t *testing.T) {
	want, err := os.ReadFile(fixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	got, err := fixtureRecord(t).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("---- got ----\n%s---- want ----\n%s", got, want)
	}
}

// Decoding the agreed bytes and writing them straight back must not move a
// single byte, or two implementations taking turns on one job churn the file
// against each other and no diff of its history means anything.
func TestTheAgreedBytesRoundTripUnchanged(t *testing.T) {
	want, err := os.ReadFile(fixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Decode(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("---- got ----\n%s---- want ----\n%s", got, want)
	}
	rs, err := r.CheckpointRanges()
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 || rs.VerifiedPrefix() != 4194304 || rs.Total() != 4194304+4194304+2097152 {
		t.Fatalf("the fixture's ranges came back as %v", rs)
	}
}

// The half of the change that makes it additive rather than a break.
//
// A reader that has never heard of `verified` decodes the checkpoint it does
// know, finds the prefix, and resumes from it. It re-fetches everything past
// the first gap, which is what it does today; it does not fail, and it does not
// trust a byte nobody proved.
func TestAReaderThatIgnoresRangesStillResumesFromThePrefix(t *testing.T) {
	body, err := os.ReadFile(fixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Decode(body)
	if err != nil {
		t.Fatalf("a record carrying ranges must be readable by a reader that ignores them: %v", err)
	}
	// Exactly what the download layer's decoder looked like before ranges
	// existed: one field, and no idea the other one is there.
	var old struct {
		VerifiedPrefix int64 `json:"verified_prefix"`
	}
	if err := r.DecodeCheckpoint(&old); err != nil {
		t.Fatal(err)
	}
	if old.VerifiedPrefix != 4194304 {
		t.Fatalf("an old reader resumes from %d, want 4194304", old.VerifiedPrefix)
	}
	// And it is never told it must understand ranges before it may proceed.
	if contains(r.Critical, ModelRanges) {
		t.Fatal("the ranges model was marked critical; every existing reader would refuse the job")
	}
	if !contains(r.Content, ModelRanges) {
		t.Fatalf("a record carrying ranges must declare them: %v", r.Content)
	}
}

// Advisory means advisory, whoever wrote the record.
func TestRangesAreNeverCriticalEvenIfAWriterSaysSo(t *testing.T) {
	r := fixtureRecord(t)
	r.Critical = append(r.Critical, ModelRanges)
	b, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Critical []string `json:"critical"`
	}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if contains(back.Critical, ModelRanges) {
		t.Fatal("a decoration was relayed as a reason for a stranger to refuse the job")
	}
}

func TestCanonicalFormMergesAndSorts(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []Range
		want []Range
	}{
		{"already canonical", []Range{{0, 4}, {8, 12}}, []Range{{0, 4}, {8, 12}}},
		{"out of order", []Range{{8, 12}, {0, 4}}, []Range{{0, 4}, {8, 12}}},
		// The case that makes the form canonical rather than merely sorted:
		// [[0,4],[4,8]] and [[0,8]] are the same proven bytes, so only one of
		// them may be legal or two implementations spell one state two ways.
		{"adjacent merge", []Range{{0, 4}, {4, 8}}, []Range{{0, 8}}},
		{"overlapping merge", []Range{{0, 6}, {4, 8}}, []Range{{0, 8}}},
		{"contained", []Range{{0, 8}, {2, 4}}, []Range{{0, 8}}},
		{"identical", []Range{{0, 8}, {0, 8}}, []Range{{0, 8}}},
		{"chain", []Range{{4, 8}, {0, 4}, {8, 9}, {20, 21}}, []Range{{0, 9}, {20, 21}}},
		// Proves nothing, so it is not part of the state and must not change
		// the bytes.
		{"empty range dropped", []Range{{0, 4}, {6, 6}}, []Range{{0, 4}}},
		{"all empty", []Range{{6, 6}}, []Range{}},
		{"nothing at all", nil, []Range{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Canonical(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(Ranges(tc.want)) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			// Canonicalising a canonical set changes nothing. If it did, one
			// state would have a spelling that depends on how many times it had
			// been written.
			again, err := Canonical(got)
			if err != nil {
				t.Fatal(err)
			}
			if !again.Equal(got) {
				t.Fatalf("not idempotent: %v then %v", got, again)
			}
		})
	}
}

func TestCanonicalRefusesNonsense(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []Range
	}{
		{"negative start", []Range{{-1, 4}}},
		{"negative end", []Range{{0, -4}}},
		{"ends before it starts", []Range{{8, 4}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Canonical(tc.in); err == nil {
				t.Fatal("accepted a range that describes no bytes")
			}
		})
	}
}

// The prefix is derived, not remembered, so it cannot drift from the set.
func TestVerifiedPrefixIsTheRangeStartingAtZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []Range
		want int64
	}{
		{"one range at zero", []Range{{0, 400}}, 400},
		{"a range at zero and others", []Range{{0, 400}, {800, 900}}, 400},
		// The case a single integer could never express, and the reason it had
		// to stop being the only thing a checkpoint says: real work is proven
		// and the prefix is still zero.
		{"nothing at zero", []Range{{800, 900}, {1000, 1200}}, 0},
		{"empty set", nil, 0},
		{"a gap closed by a merge", []Range{{0, 400}, {400, 800}}, 800},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs, err := Canonical(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if got := rs.VerifiedPrefix(); got != tc.want {
				t.Fatalf("prefix %d, want %d", got, tc.want)
			}
			// And what is written says the same thing as what is computed.
			raw, err := CheckpointJSONWithRanges(nil, rs)
			if err != nil {
				t.Fatal(err)
			}
			var cp struct {
				VerifiedPrefix int64 `json:"verified_prefix"`
			}
			if err := json.Unmarshal(raw, &cp); err != nil {
				t.Fatal(err)
			}
			if cp.VerifiedPrefix != tc.want {
				t.Fatalf("wrote prefix %d, want %d", cp.VerifiedPrefix, tc.want)
			}
		})
	}
}

// A checkpoint written before ranges existed IS a range set — one range
// starting at zero. Without this the degenerate case would be a special case,
// and every caller would have to handle both.
func TestAPrefixOnlyCheckpointReadsAsOneRange(t *testing.T) {
	rs, err := RangesFromCheckpointJSON([]byte(`{"verified_prefix":400}`))
	if err != nil {
		t.Fatal(err)
	}
	if !rs.Equal(Ranges{{0, 400}}) {
		t.Fatalf("got %v, want one range [0,400)", rs)
	}

	for _, empty := range []string{`{}`, `{"verified_prefix":0}`, `null`, ``} {
		rs, err := RangesFromCheckpointJSON([]byte(empty))
		if err != nil {
			t.Fatalf("%q: %v", empty, err)
		}
		if len(rs) != 0 {
			t.Fatalf("%q gave %v, want nothing proven", empty, rs)
		}
	}
}

// A prefix-only writer that took the job over and advanced the prefix without
// touching `verified` leaves a record where the two disagree. Both fields are
// claims that bytes are PROVEN and neither is a claim that other bytes are not,
// so the union is the only reading that loses nothing.
func TestAStalerVerifiedSetDoesNotLoseTheProvenPrefix(t *testing.T) {
	rs, err := RangesFromCheckpointJSON([]byte(
		`{"verified_prefix":8388608,"verified":[[0,4194304],[16777216,20971520]]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !rs.Equal(Ranges{{0, 8388608}, {16777216, 20971520}}) {
		t.Fatalf("got %v; the prefix a predecessor proved was thrown away", rs)
	}
}

func TestACheckpointWithRangesIsRefusedIfItIsNotRanges(t *testing.T) {
	for _, bad := range []string{
		`{"verified":[[0]]}`,
		`{"verified":[[0,4,8]]}`,
		`{"verified":[[4,0]]}`,
		`{"verified":[[-1,4]]}`,
		`{"verified":"nope"}`,
	} {
		if _, err := RangesFromCheckpointJSON([]byte(bad)); err == nil {
			t.Fatalf("%s was accepted as a set of proven ranges", bad)
		}
	}
}

// What a parallel fetcher actually does: sixteen parts land in whatever order
// they land, and each is recorded as it is proven.
func TestPartsLandingOutOfOrderAccumulate(t *testing.T) {
	var r Record
	r.Kind = "download"
	if err := r.SetSpec(map[string]any{"bytes": 64}); err != nil {
		t.Fatal(err)
	}
	for _, part := range []int64{5, 0, 2, 1, 3, 4} {
		if err := r.AddCheckpointRange(part*8, part*8+8); err != nil {
			t.Fatal(err)
		}
	}
	rs, err := r.CheckpointRanges()
	if err != nil {
		t.Fatal(err)
	}
	// Six touching parts are one proven range, and the prefix is the whole file.
	if !rs.Equal(Ranges{{0, 48}}) {
		t.Fatalf("got %v, want one merged range", rs)
	}
	if rs.VerifiedPrefix() != 48 {
		t.Fatalf("prefix %d", rs.VerifiedPrefix())
	}

	// And the state that has no representation without this change: parts 0, 2
	// and 5 done, the rest not.
	var s Record
	s.Kind = "download"
	if err := s.SetSpec(map[string]any{"bytes": 48}); err != nil {
		t.Fatal(err)
	}
	for _, part := range []int64{0, 2, 5} {
		if err := s.AddCheckpointRange(part*8, part*8+8); err != nil {
			t.Fatal(err)
		}
	}
	rs, err = s.CheckpointRanges()
	if err != nil {
		t.Fatal(err)
	}
	if !rs.Equal(Ranges{{0, 8}, {16, 24}, {40, 48}}) {
		t.Fatalf("got %v", rs)
	}
	if rs.VerifiedPrefix() != 8 {
		t.Fatalf("prefix %d, want only part 0", rs.VerifiedPrefix())
	}
	// The gaps are what is left to fetch, which is the question a resume asks.
	if got := rs.Missing(0, 48); !got.Equal(Ranges{{8, 16}, {24, 40}}) {
		t.Fatalf("missing %v", got)
	}
}

func TestCoversAndMissing(t *testing.T) {
	rs := Ranges{{0, 8}, {16, 24}}
	for _, tc := range []struct {
		start, end int64
		want       bool
	}{
		{0, 8, true},
		{2, 6, true},
		{0, 9, false},
		{8, 16, false},
		{16, 24, true},
		{4, 4, true}, // an empty interval needs nothing proven
		{100, 100, true},
	} {
		if got := rs.Covers(tc.start, tc.end); got != tc.want {
			t.Fatalf("Covers(%d,%d) = %v", tc.start, tc.end, got)
		}
	}
	if got := rs.Missing(0, 32); !got.Equal(Ranges{{8, 16}, {24, 32}}) {
		t.Fatalf("missing %v", got)
	}
	var nothing Ranges
	if got := nothing.Missing(0, 32); !got.Equal(Ranges{{0, 32}}) {
		t.Fatalf("nothing proven should leave everything to fetch, got %v", got)
	}
	if got := rs.Missing(0, 4); len(got) != 0 {
		t.Fatalf("missing %v, want nothing", got)
	}
}

// A checkpoint belongs to whoever writes it. These helpers own two keys and
// must leave the rest exactly where they were — sorted, so all three
// implementations put them in the same order.
func TestOtherCheckpointKeysSurviveAndAreOrdered(t *testing.T) {
	raw, err := CheckpointJSONWithRanges(
		[]byte(`{"zebra":1,"apple":{"nested":[1,2]},"verified_prefix":99}`),
		Ranges{{0, 400}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"verified_prefix":400,"verified":[[0,400]],"apple":{"nested":[1,2]},"zebra":1}`
	if string(raw) != want {
		t.Fatalf("got  %s\nwant %s", raw, want)
	}
}

// The declaration is carried rather than derived, so it has to be withdrawable:
// a record that kept declaring a model whose data it no longer holds sends a
// reader looking for something that is not there.
func TestClearingRangesWithdrawsTheDeclaration(t *testing.T) {
	r := fixtureRecord(t)
	if err := r.ClearCheckpointRanges(); err != nil {
		t.Fatal(err)
	}
	b, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), ModelRanges) {
		t.Fatalf("the declaration outlived the data:\n%s", b)
	}
	if strings.Contains(string(b), `"verified"`) {
		t.Fatalf("the ranges were left behind:\n%s", b)
	}
	// The prefix survives, because it is not ours to remove: an old reader
	// still resumes from it.
	if !strings.Contains(string(b), `"verified_prefix": 4194304`) {
		t.Fatalf("clearing the ranges took the prefix with it:\n%s", b)
	}
}

// The declaration cannot be derived — the checkpoint is opaque here — so it has
// to survive a reader that does a full read, modify and write without ever
// looking inside one.
func TestTheDeclarationSurvivesAReadModifyWrite(t *testing.T) {
	s := openStore(t)
	r := fixtureRecord(t)
	r.ID = ""
	r.Lease = Lease{}
	r.State = StatePending
	id, err := s.Submit(*r)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(id, "somebody-else", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(id, claimed.Lease.Epoch, func(rr *Record) error {
		rr.Progress.Done = 99 // nothing to do with ranges
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(got.Content, ModelRanges) {
		t.Fatalf("a reader that never looked at the checkpoint dropped its declaration: %v", got.Content)
	}
	rs, err := got.CheckpointRanges()
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 {
		t.Fatalf("the ranges did not survive: %v", rs)
	}
}
