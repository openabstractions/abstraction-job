package job

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// The two cases an outside reviewer reproduced on 2026-09-09, adapted into this
// tree and kept. His probes were TestImmutableSpec, over file, memory and
// remote, and TestMemoryReadMutation.
//
// What is worth keeping about them is not the assertions, which are obvious. It
// is that the contract had said `spec` was immutable since the record existed,
// every test in this package passed, and all three bindings did something
// different with the same call: two wrote the change, one threw it away. Nothing
// here asked the three of them the same question, so nothing could see it.

// reasonOf strips the wrapping each binding adds, so "the same refusal" is a
// comparison rather than an impression: the service binding names the error
// class on the wire and re-wraps it on arrival, which spells one refusal twice.
func reasonOf(err error) string {
	s := err.Error()
	for {
		trimmed := strings.TrimPrefix(s, ErrInvalid.Error()+": ")
		if trimmed == s {
			return s
		}
		s = trimmed
	}
}

func TestNoBindingLetsALeaseMoveWhatTheWorkIs(t *testing.T) {
	for _, move := range []struct {
		field  string
		change func(*Record)
	}{
		{"spec", func(r *Record) { r.SetSpec(map[string]string{"input": "changed"}) }},
		{"kind", func(r *Record) { r.Kind = "something-else" }},
		{"id", func(r *Record) { r.ID = NewID() }},
		{"created_at", func(r *Record) { r.CreatedAt = At(r.CreatedAt.Add(-time.Hour)) }},
		{"envelope", func(r *Record) { r.Envelope = &Envelope{Schema: vendor, Actions: []string{ActionPause}} }},
	} {
		t.Run(move.field, func(t *testing.T) {
			var reason, first string
			for _, b := range bindings(t) {
				r := Record{Kind: "review"}
				if err := r.SetSpec(map[string]string{"input": "original"}); err != nil {
					t.Fatal(err)
				}
				id, err := b.store.Submit(r)
				if err != nil {
					t.Fatal(err)
				}
				held, err := b.store.Claim(id, "review", time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				_, err = b.store.Update(id, held.Lease.Epoch, func(rr *Record) error {
					move.change(rr)
					return nil
				})
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("%s: a lease holder moved %s: %v", b.name, move.field, err)
				}
				t.Logf("%s refuses with: %s", b.name, reasonOf(err))
				if reason == "" {
					reason, first = reasonOf(err), b.name
				} else if got := reasonOf(err); got != reason {
					t.Fatalf("%s refuses with %q, %s with %q — one call, two answers",
						first, reason, b.name, got)
				}

				got, err := b.store.Load(id)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(got.Spec), "original") || got.Kind != "review" || got.ID != id {
					t.Fatalf("%s: the refusal did not roll back: %s %s %s", b.name, got.ID, got.Kind, got.Spec)
				}
				if got.Envelope != nil {
					t.Fatalf("%s: an envelope arrived through a refused write", b.name)
				}

				if _, err := b.store.Update(id, held.Lease.Epoch, func(rr *Record) error {
					rr.Progress.Done = 7
					return nil
				}); err != nil {
					t.Fatalf("%s: an ordinary write was refused: %v", b.name, err)
				}
			}
		})
	}
}

// A lease holder still owns everything a lease is for. Without this the rule
// above could be satisfied by refusing every write.
func TestALeaseStillWritesWhatALeaseIsFor(t *testing.T) {
	for _, b := range bindings(t) {
		r := Record{Kind: "review", Requires: []string{"survives_process_exit"}}
		if err := r.SetSpec(map[string]string{"input": "original"}); err != nil {
			t.Fatal(err)
		}
		id, err := b.store.Submit(r)
		if err != nil {
			t.Fatal(err)
		}
		held, err := b.store.Claim(id, "review", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		got, err := b.store.Update(id, held.Lease.Epoch, func(rr *Record) error {
			rr.Progress.Done = 512
			rr.Progress.Step = &Step{Name: "copying from nas", Ordinal: 2, Of: 3}
			rr.Error = "a retry ago"
			rr.Extensions = map[string]json.RawMessage{"review/extra@1": json.RawMessage(`{"n":1}`)}
			return rr.SetCheckpoint(map[string]int64{"verified_prefix": 512})
		})
		if err != nil {
			t.Fatalf("%s: %v", b.name, err)
		}
		if got.Progress.Done != 512 || got.Progress.Step == nil || got.Error == "" ||
			len(got.Checkpoint) == 0 || len(got.Extensions) != 1 {
			t.Fatalf("%s: a lease holder's own fields did not land: %+v", b.name, got)
		}
	}
}

// Every binding hands the caller something the caller owns. MemoryStore is the
// one that can get this wrong, and did: it kept the stored map.
func TestNoBindingHandsOutItsOwnState(t *testing.T) {
	for _, b := range bindings(t) {
		r := Record{
			Kind:       "review",
			Requires:   []string{"survives_process_exit"},
			Extensions: map[string]json.RawMessage{"review/extra@1": json.RawMessage(`{"n":1}`)},
		}
		r.Progress.Step = &Step{Name: "fetching", Ordinal: 1, Of: 2}
		if err := r.SetSpec(map[string]string{"input": "original"}); err != nil {
			t.Fatal(err)
		}
		if err := r.SetCheckpoint(map[string]int64{"verified_prefix": 1}); err != nil {
			t.Fatal(err)
		}
		id, err := b.store.Submit(r)
		if err != nil {
			t.Fatal(err)
		}

		// The record the caller submitted is the caller's, and the store keeps
		// none of it: writing through the map handed to Submit is the same
		// unleased write as writing through the one Load returns.
		r.Extensions["review/extra@1"] = json.RawMessage(`{"n":98}`)
		r.Progress.Step.Name = "submitted-then-renamed"

		held, err := b.store.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		held.Extensions["review/extra@1"] = json.RawMessage(`{"n":2}`)
		held.Extensions["review/added@1"] = json.RawMessage(`{}`)
		held.Progress.Step.Name = "renamed"
		held.Requires[0] = "nothing"
		held.Content[0] = "not-a-feature"
		held.Critical[0] = "not-a-feature"
		// The byte slice INSIDE an extension value, not just the map: a copy of
		// the map alone leaves every value's backing array shared.
		second, err := b.store.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		copy(second.Extensions["review/extra@1"], []byte(`{"n":9}`))
		copy(second.Spec, []byte(`{"input":"XXXXXXXX"}`))
		copy(second.Checkpoint, []byte(`{"verified_prefix":9}`))

		again, err := b.store.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		if !sameOpaque(again.Extensions["review/extra@1"], []byte(`{"n":1}`)) {
			t.Fatalf("%s: an unleased write reached the store: %s", b.name, again.Extensions["review/extra@1"])
		}
		if _, added := again.Extensions["review/added@1"]; added {
			t.Fatalf("%s: an unleased extension reached the store", b.name)
		}
		if again.Progress.Step.Name != "fetching" || again.Requires[0] != "survives_process_exit" {
			t.Fatalf("%s: an unleased write reached the store: %+v", b.name, again)
		}
		if again.Content[0] != FeatureBase || again.Critical[0] != FeatureBase {
			t.Fatalf("%s: the declaration was written through: %v %v", b.name, again.Content, again.Critical)
		}
		if !strings.Contains(string(again.Spec), "original") || strings.Contains(string(again.Checkpoint), ":9") {
			t.Fatalf("%s: an opaque payload was written through: %s %s", b.name, again.Spec, again.Checkpoint)
		}
	}
}

// A mutation that changes shared data and then fails must leave nothing behind.
// The reviewer named this case and did not reproduce it; it is the same aliasing
// as the one above, reached through Update instead of Load.
func TestAFailedUpdateLeavesTheStoreUnchanged(t *testing.T) {
	sentinel := errors.New("the mutation changed its mind")
	for _, b := range bindings(t) {
		r := Record{Kind: "review", Extensions: map[string]json.RawMessage{"review/extra@1": json.RawMessage(`{"n":1}`)}}
		r.Progress.Step = &Step{Name: "fetching", Ordinal: 1}
		if err := r.SetSpec(map[string]string{"input": "original"}); err != nil {
			t.Fatal(err)
		}
		id, err := b.store.Submit(r)
		if err != nil {
			t.Fatal(err)
		}
		held, err := b.store.Claim(id, "review", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		was, err := b.store.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		// Read out as values BEFORE the update, never as a record to compare
		// against afterwards: a store that hands out its own state hands out the
		// same state twice, and the two views agree while both have moved.
		before := struct {
			done      int64
			step      string
			extension string
			spec      string
			updatedAt time.Time
		}{was.Progress.Done, was.Progress.Step.Name, string(was.Extensions["review/extra@1"]), string(was.Spec), was.UpdatedAt.Time}

		_, err = b.store.Update(id, held.Lease.Epoch, func(rr *Record) error {
			rr.Progress.Done = 999
			rr.Progress.Step.Name = "half-written"
			rr.Extensions["review/extra@1"] = json.RawMessage(`{"n":3}`)
			copy(rr.Spec, []byte(`{"input":"XXXXXXXX"}`))
			return sentinel
		})
		if !errors.Is(err, sentinel) && !strings.Contains(err.Error(), sentinel.Error()) {
			t.Fatalf("%s: expected the mutation's own error, got %v", b.name, err)
		}
		after, err := b.store.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		if after.Progress.Done != before.done ||
			after.Progress.Step.Name != before.step ||
			string(after.Extensions["review/extra@1"]) != before.extension ||
			!sameOpaque(after.Spec, []byte(before.spec)) ||
			!after.UpdatedAt.Time.Equal(before.updatedAt) {
			t.Fatalf("%s: a failed update left something behind: %+v", b.name, after)
		}
	}
}

// The whitespace an opaque payload carries is the record format's, not the
// payload's: the same spec is indented on disk and compact on the wire, so a
// rule that compared raw bytes would refuse every write over the service
// binding. Escapes and numbers are still compared exactly.
func TestOpaqueComparisonIgnoresTheFormatsOwnWhitespace(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{`{"n":1}`, "{\n  \"n\": 1\n}", true},
		{`{"n":1}`, `{"n":1.0}`, false},
		{`{"n":1e2}`, `{"n":100}`, false},
		{`{"s":"a b"}`, `{"s":"ab"}`, false},
		{"{\"s\":\"a\\u0026b\"}", `{"s":"a&b"}`, false},
		{`{"n":1}`, `not json`, false},
	} {
		if got := sameOpaque([]byte(c.a), []byte(c.b)); got != c.same {
			t.Fatalf("sameOpaque(%s, %s) = %v", c.a, c.b, got)
		}
	}
}
