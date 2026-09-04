package job

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The rule that makes extensions worth having, and the one easiest to omit.
//
// A reader that drops an unknown extension on write destroys another
// participant's data, and the loss is invisible precisely because nobody here
// can read it: no error, no warning, and the writer finds out much later that
// the thing it recorded is gone. Kubernetes learned this with field pruning;
// there is no reason to learn it again.
func TestAnUnknownExtensionSurvivesAReaderThatCannotReadIt(t *testing.T) {
	s := openStore(t)

	var r Record
	r.Kind = "test"
	if err := r.SetSpec(map[string]any{"anything": 1}); err != nil {
		t.Fatal(err)
	}
	// Written by somebody else, in a schema this implementation has never heard
	// of and never will.
	r.Extensions = map[string]json.RawMessage{
		"someone.else/v3": json.RawMessage(`{"deep":{"nested":[1,2,3]},"why":"because"}`),
	}
	id, err := s.Submit(r)
	if err != nil {
		t.Fatal(err)
	}

	// A full read/modify/write cycle by a reader that does not understand the
	// key: claim it, change something else entirely, hand it back.
	claimed, err := s.Claim(id, "a-stranger", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(id, claimed.Lease.Epoch, func(rr *Record) error {
		rr.Progress.Done = 99
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := got.Extensions["someone.else/v3"]
	if !ok {
		t.Fatal("the extension was dropped by a reader that could not read it")
	}
	var back struct {
		Deep struct {
			Nested []int `json:"nested"`
		} `json:"deep"`
		Why string `json:"why"`
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("the extension came back malformed: %v", err)
	}
	if len(back.Deep.Nested) != 3 || back.Why != "because" {
		t.Fatalf("the extension came back changed: %s", raw)
	}
}

// An extension is optional to everyone who does not know it. Anything a
// stranger MUST obey does not belong here — which is why a stop request went
// into the record as Intent instead.
func TestAnExtensionNeedsANameAndValidJSON(t *testing.T) {
	var r Record
	r.Content = []string{ModelBase}
	r.Critical = []string{ModelBase}
	r.State = StatePending
	r.Kind = "test"
	r.ID = "1700000000000-abc"
	if err := r.SetSpec(map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}

	r.Extensions = map[string]json.RawMessage{"  ": json.RawMessage(`{}`)}
	if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "needs a name") {
		t.Fatalf("an unnamed extension was accepted: %v", err)
	}

	r.Extensions = map[string]json.RawMessage{"x/v1": json.RawMessage(`{not json`)}
	if err := r.Validate(); err == nil {
		t.Fatal("an extension that is not JSON was accepted; it cannot be relayed")
	}
}
