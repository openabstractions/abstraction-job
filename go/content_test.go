package job

import (
	"errors"
	"strings"
	"testing"
)

// The whole reason a named content set replaced a version number.
//
// With an integer, "I do not know version 6" was the only thing a reader could
// say, so refusing everything was the only safe response — including for the
// overwhelmingly common case where the addition was decoration it could have
// ignored. Here the question is per-feature, and the answer differs.
func TestAnUnknownAdvisoryFeatureIsReadAndAnUnknownCriticalOneIsNot(t *testing.T) {
	// Written by a future implementation that has a feature this one has never
	// heard of. It is in content but NOT in critical, so it is decoration.
	future := []byte(`{
  "content": [
    "abstraction.job/base@1",
    "abstraction.job/hovercraft@7"
  ],
  "critical": [
    "abstraction.job/base@1"
  ],
  "id": "1700000000000-abc",
  "kind": "test",
  "state": "running",
  "spec": {"what": "anything"},
  "progress": {"done": 0, "updated_at": "2026-01-01T00:00:00.000000Z"},
  "lease": {"owner": "", "epoch": 0, "expires_at": "2026-01-01T00:00:00.000000Z"},
  "created_at": "2026-01-01T00:00:00.000000Z",
  "updated_at": "2026-01-01T00:00:00.000000Z"
}`)
	r, err := Decode(future)
	if err != nil {
		t.Fatalf("a record whose only unknown feature is advisory must still be read: %v", err)
	}
	if r.State != StateRunning {
		t.Fatalf("state %q", r.State)
	}

	// The same record, with that feature declared critical. Now the writer is
	// saying "you cannot act on this correctly without understanding me", and
	// the only honest answer is to refuse.
	critical := strings.Replace(string(future),
		`"critical": [
    "abstraction.job/base@1"
  ]`,
		`"critical": [
    "abstraction.job/base@1",
    "abstraction.job/hovercraft@7"
  ]`, 1)
	if _, err := Decode([]byte(critical)); !errors.Is(err, ErrUnknownSchema) {
		t.Fatalf("a record requiring an unknown feature was accepted: %v", err)
	}
}

// Stores full of the old integer exist on real disks and on a NAS. The mapping
// onto features is exact rather than a guess, so nothing is assumed about what
// those records contain — and refusing them would orphan work in flight.
func TestLegacyVersionedRecordsStillRead(t *testing.T) {
	for _, tc := range []struct {
		schema   int
		wants    Want
		contains string
	}{
		{3, WantRun, FeatureBase},
		{4, WantRun, FeatureIntent},
	} {
		body := `{
  "schema": ` + itoa(tc.schema) + `,
  "id": "1700000000000-abc",
  "kind": "test",
  "state": "running",
  "spec": {"what": "anything"},
  "progress": {"done": 0, "updated_at": "2026-01-01T00:00:00.000000Z"},
  "lease": {"owner": "", "epoch": 0, "expires_at": "2026-01-01T00:00:00.000000Z"},
  "created_at": "2026-01-01T00:00:00.000000Z",
  "updated_at": "2026-01-01T00:00:00.000000Z"
}`
		r, err := Decode([]byte(body))
		if err != nil {
			t.Fatalf("schema %d must still be readable: %v", tc.schema, err)
		}
		if r.Wants() != tc.wants {
			t.Fatalf("schema %d wants %q", tc.schema, r.Wants())
		}
		if !contains(r.Content, tc.contains) {
			t.Fatalf("schema %d should imply %s, got %v", tc.schema, tc.contains, r.Content)
		}

		// And written back in the current form, with the integer gone.
		b, err := r.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), `"schema"`) {
			t.Fatalf("schema %d was written back carrying the legacy integer", tc.schema)
		}
	}

	// A version this format never had is refused rather than guessed at.
	bad := `{"schema": 99, "id": "x", "kind": "t", "state": "running", "spec": {},
  "progress": {"done": 0, "updated_at": "2026-01-01T00:00:00.000000Z"},
  "lease": {"owner": "", "epoch": 0, "expires_at": "2026-01-01T00:00:00.000000Z"},
  "created_at": "2026-01-01T00:00:00.000000Z", "updated_at": "2026-01-01T00:00:00.000000Z"}`
	if _, err := Decode([]byte(bad)); !errors.Is(err, ErrUnknownSchema) {
		t.Fatalf("an invented legacy version was accepted: %v", err)
	}
}

// A step is advisory, so a record carrying one must never mark it critical:
// that would make a decoration into a reason for a stranger to refuse the job.
func TestAStepIsNeverCritical(t *testing.T) {
	s := openStore(t)
	var r Record
	r.Kind = "test"
	if err := r.SetSpec(map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}
	r.Progress.Step = &Step{Name: "copying from nas", Ordinal: 2, Of: 3, Done: 5, Total: 10}
	id, err := s.Submit(r)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(got.Content, FeatureStep) {
		t.Fatalf("a record with a step must declare it: %v", got.Content)
	}
	if contains(got.Critical, FeatureStep) {
		t.Fatal("a step is advisory and must never be critical")
	}
	if got.Progress.Step == nil || got.Progress.Step.Name != "copying from nas" {
		t.Fatalf("the step did not survive: %+v", got.Progress.Step)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
