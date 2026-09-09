package job

import (
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

const vendor = "nas.example/transfer@2"

func withEnvelope(t *testing.T, e *Envelope) *Record {
	t.Helper()
	var r Record
	r.ID = "1787202430967-a752f9a9c2c77b123ffd"
	r.Kind = "test"
	r.State = StatePending
	r.Envelope = e
	r.Progress.UpdatedAt = At(time.Unix(0, 0).UTC())
	r.Lease.ExpiresAt = At(time.Unix(0, 0).UTC())
	r.CreatedAt = At(time.Unix(0, 0).UTC())
	r.UpdatedAt = At(time.Unix(0, 0).UTC())
	if err := r.SetSpec(map[string]any{"anything": 1}); err != nil {
		t.Fatal(err)
	}
	return &r
}

// A schema identifier is a NAME. The grammar is the safety argument, so this is
// the test that has to hold: not that we decline to fetch an address, but that
// an address is not a legal value in the first place.
func TestASchemaIdentifierCannotBeAnAddress(t *testing.T) {
	refused := []string{
		"https://example.invalid/steal@1",
		"http://127.0.0.1:8080/x@1",
		"file:///etc/passwd@1",
		"//example.invalid/x@1",
		`\\host\share\x@1`,
		"../../../etc/passwd@1",
		"C:/windows/system32@1",
		"example.invalid/x@1?callback=http://evil",
		"example.invalid/%2e%2e/x@1",
		"example.invalid/x@1#fragment",
		"example.invalid/a/b@1",
		"data:text/plain;base64,aGk=@1",
		"Example.Invalid/X@1",
		"example.invalid/x",
		"example.invalid/x@0",
		"example.invalid/x@01",
		"example.invalid/x@v1",
		"example.invalid//x@1",
		"example..invalid/x@1",
		"-example.invalid/x@1",
		"example.invalid/-x@1",
		"example invalid/x@1",
		"example.invalid/x@1\nschema: other/x@1",
		"",
		strings.Repeat("a", 130) + "/x@1",
	}
	for _, s := range refused {
		if ValidSchema(s) {
			t.Errorf("%q is accepted as a schema identifier and can be dereferenced", s)
		}
	}
	for _, s := range []string{
		"abstraction.job/base@1",
		"nas.example/transfer@2",
		"a/b@1",
		"a-b.c-d.e/f-g@123456789",
	} {
		if !ValidSchema(s) {
			t.Errorf("%q is a schema identifier and was refused", s)
		}
	}
}

// The demonstration, rather than the assertion: a listener nobody is allowed to
// reach, its own address written into the record in every disguise, and a count
// of the connections that arrived.
//
// Reading a record is the whole surface — decode, validate, encode, ask — and
// none of it may open anything. The listener is closed before the count is read,
// so the accept loop has finished and there is nothing to wait for.
func TestNothingDereferencesASchemaIdentifier(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	reached := make(chan int)
	go func() {
		n := 0
		for {
			c, err := ln.Accept()
			if err != nil {
				reached <- n
				return
			}
			n++
			c.Close()
		}
	}()
	addr := ln.Addr().String()

	for _, s := range []string{
		"http://" + addr + "/schema@1",
		"//" + addr + "/schema@1",
		addr + "/schema@1",
	} {
		r := withEnvelope(t, &Envelope{Schema: s})
		if _, err := r.Encode(); !errors.Is(err, ErrInvalid) {
			t.Errorf("schema %q: want ErrInvalid, got %v", s, err)
		}
	}

	// And the legal case, taken through everything a supervisor does with a
	// record it did not create.
	r := withEnvelope(t, &Envelope{Schema: vendor, Actions: []string{ActionPause, vendor + "#verify"}})
	b, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	_ = back.Supports(vendor + "#verify")
	_ = back.Ask(vendor+"#verify", Supervisor{Schemas: []string{vendor}, Actions: []string{vendor + "#verify"}})
	_ = back.Ask("pause", Supervisor{})

	ln.Close()
	if n := <-reached; n != 0 {
		t.Fatalf("reading a record opened %d connection(s) to an address inside it", n)
	}
}

// An unknown schema is REPORTED. A supervisor that declines in silence is
// indistinguishable from one that quietly did the wrong thing.
func TestAnUnknownSchemaIsRefusedAndNamed(t *testing.T) {
	r := withEnvelope(t, &Envelope{Schema: vendor, Actions: []string{vendor + "#verify"}})
	err := r.Ask(vendor+"#verify", Supervisor{
		Schemas: []string{"someone.else/other@1"},
		Actions: []string{vendor + "#verify"},
	})
	if !errors.Is(err, ErrUnknownSchema) {
		t.Fatalf("want ErrUnknownSchema, got %v", err)
	}
	if !strings.Contains(err.Error(), vendor) {
		t.Fatalf("the refusal does not name what was refused: %v", err)
	}
}

func TestAnUnknownActionIsRefused(t *testing.T) {
	r := withEnvelope(t, &Envelope{Schema: vendor, Actions: []string{ActionPause}})
	known := Supervisor{Schemas: []string{vendor}, Actions: []string{ActionPause, vendor + "#verify"}}

	// The kind does not declare it.
	if err := r.Ask(vendor+"#verify", known); !errors.Is(err, ErrNotSupported) {
		t.Errorf("an action the kind does not declare: want ErrNotSupported, got %v", err)
	}
	// The kind declares it and this supervisor has not built it.
	r2 := withEnvelope(t, &Envelope{Schema: vendor, Actions: []string{vendor + "#remirror"}})
	if err := r2.Ask(vendor+"#remirror", known); !errors.Is(err, ErrNotSupported) {
		t.Errorf("an action this supervisor does not implement: want ErrNotSupported, got %v", err)
	}
	// A record with no envelope declares nothing.
	r3 := withEnvelope(t, nil)
	if err := r3.Ask("pause", known); !errors.Is(err, ErrNotSupported) {
		t.Errorf("a record with no envelope: want ErrNotSupported, got %v", err)
	}
	// Not an action name at all.
	if err := r.Ask("Pause!", known); !errors.Is(err, ErrInvalid) {
		t.Errorf("want ErrInvalid, got %v", err)
	}
}

// Two vendors will both define "cancel". A qualified name and a bare one are
// different actions and never match each other.
func TestANamespacedActionDoesNotCollideWithABareOne(t *testing.T) {
	r := withEnvelope(t, &Envelope{Schema: vendor, Actions: []string{vendor + "#cancel"}})
	if r.Supports(ActionCancel) {
		t.Error("a bare cancel matched a vendor's cancel")
	}
	if !r.Supports(vendor + "#cancel") {
		t.Error("the vendor's own cancel does not match itself")
	}

	both := withEnvelope(t, &Envelope{Schema: vendor, Actions: []string{ActionCancel, vendor + "#cancel"}})
	if !both.Supports(ActionCancel) || !both.Supports(vendor+"#cancel") {
		t.Error("a record may declare both and they are two actions")
	}
	if _, err := both.Encode(); err != nil {
		t.Fatalf("declaring both is legal: %v", err)
	}
}

func TestAnActionNameResolvesToASchemaTheRecordDeclares(t *testing.T) {
	cases := []struct {
		why     string
		actions []string
	}{
		{"a bare name outside this layer's vocabulary squats a name we may later define",
			[]string{"remirror"}},
		{"a base action spelled the long way is a second spelling of one action",
			[]string{BaseSchema + "#pause"}},
		{"a schema this record does not declare is a namespace it has no claim to",
			[]string{"someone.else/other@1#verify"}},
		{"a name declared twice", []string{ActionPause, ActionPause}},
		{"not an action name", []string{"Verify"}},
		{"an address wearing an action's clothes", []string{"https://evil.invalid/x@1#go"}},
		{"two separators", []string{vendor + "#a#b"}},
	}
	for _, c := range cases {
		r := withEnvelope(t, &Envelope{Schema: vendor, Actions: c.actions})
		if _, err := r.Encode(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", c.why, err)
		}
	}

	ok := withEnvelope(t, &Envelope{Schema: vendor, Actions: []string{
		ActionPause, ActionResume, ActionCancel, ActionRecall, vendor + "#verify", vendor + "#re-mirror",
	}})
	if _, err := ok.Encode(); err != nil {
		t.Fatalf("every one of these resolves: %v", err)
	}
}

// A record that carries an envelope says so, and says a reader that cannot read
// one must not carry on: a reader that ignored it would write the record back
// without it, and the thing destroyed would be the description.
func TestAnEnvelopeIsDeclaredAndCritical(t *testing.T) {
	r := withEnvelope(t, &Envelope{Schema: vendor})
	if _, err := r.Encode(); err != nil {
		t.Fatal(err)
	}
	if !contains(r.Content, FeatureEnvelope) {
		t.Error("content does not name the envelope")
	}
	if !contains(r.Critical, FeatureEnvelope) {
		t.Error("the envelope is not marked critical")
	}
	plain := withEnvelope(t, nil)
	if _, err := plain.Encode(); err != nil {
		t.Fatal(err)
	}
	if contains(plain.Content, FeatureEnvelope) {
		t.Error("a record with no envelope declares one")
	}
}

// A record already on a disk somewhere has no envelope, and must load.
func TestARecordWrittenBeforeTheEnvelopeExistedStillLoads(t *testing.T) {
	const old = `{
  "content": [
    "abstraction.job/base@1"
  ],
  "critical": [
    "abstraction.job/base@1"
  ],
  "id": "1787202430967-a752f9a9c2c77b123ffd",
  "kind": "download",
  "state": "running",
  "spec": {
    "artifact": {
      "bytes": 23068672
    }
  },
  "progress": {
    "done": 10485760,
    "total": 23068672,
    "updated_at": "2026-08-20T05:07:14.951609Z"
  },
  "lease": {
    "owner": "go-worker",
    "epoch": 2,
    "expires_at": "2026-08-20T05:08:14.635068Z"
  },
  "created_at": "2026-08-20T05:07:10.967343Z",
  "updated_at": "2026-08-20T05:07:15.134811Z"
}
`
	r, err := Decode([]byte(old))
	if err != nil {
		t.Fatalf("a record from before this field existed was refused: %v", err)
	}
	if r.Envelope != nil {
		t.Fatal("an envelope appeared from nowhere")
	}
	if r.Schema() != "" || r.Actions() != nil {
		t.Fatal("a record that says nothing was read as saying something")
	}
	// And it goes back out exactly as it came in: adding an optional field to
	// the definition changed no byte of a record that does not use it.
	back, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != old {
		t.Fatalf("re-encoding an old record changed its bytes:\n%s", back)
	}
}

// A supported action is a property of the KIND. If it were instance state,
// a supervisor would try to pause a job whose worker died an hour ago.
//
// Every binding, because a rule one binding refuses and another silently
// ignores is not a rule: the same application on two bindings is the test that
// decides whether this is an abstraction at all.
func TestALeaseHolderCannotMoveTheEnvelope(t *testing.T) {
	for _, b := range bindings(t) {
		s := b.store
		r := *withEnvelope(t, &Envelope{Schema: vendor, Actions: []string{ActionPause}})
		id, err := s.Submit(r)
		if err != nil {
			t.Fatal(err)
		}
		held, err := s.Claim(id, "a-holder", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		for _, change := range []func(*Record){
			func(rr *Record) { rr.Envelope.Actions = append(rr.Envelope.Actions, vendor+"#verify") },
			func(rr *Record) { rr.Envelope.Actions = nil },
			func(rr *Record) { rr.Envelope.Schema = "someone.else/other@1" },
			func(rr *Record) { rr.Envelope = nil },
		} {
			_, err := s.Update(id, held.Lease.Epoch, func(rr *Record) error {
				change(rr)
				return nil
			})
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("%s: a lease holder moved the envelope: %v", b.name, err)
			}
		}
		// The record still says what it said.
		got, err := s.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Schema() != vendor || len(got.Actions()) != 1 {
			t.Fatalf("the envelope moved anyway: %+v", got.Envelope)
		}
		// A write that leaves it alone still works.
		if _, err := s.Update(id, held.Lease.Epoch, func(rr *Record) error {
			rr.Progress.Done = 7
			return nil
		}); err != nil {
			t.Fatalf("an ordinary write was refused: %v", err)
		}
	}
}

func TestAnEnvelopeRoundTrips(t *testing.T) {
	r := withEnvelope(t, &Envelope{Schema: vendor, Actions: []string{ActionPause, vendor + "#verify"}})
	first, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Decode(first)
	if err != nil {
		t.Fatal(err)
	}
	second, err := back.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("decode then encode changed the bytes:\n%s\n%s", first, second)
	}
	if !strings.Contains(string(first), "\"envelope\": {\n    \"schema\": \"nas.example/transfer@2\",\n") {
		t.Fatalf("the envelope is not where the definition puts it:\n%s", first)
	}
}

// The envelope refuses a field it does not know, like every other part of the
// record: a newer writer's addition here changes what may be asked of a job, and
// a reader that skipped it would be answering an older question.
func TestTheEnvelopeRefusesAnUnknownField(t *testing.T) {
	var doc map[string]json.RawMessage
	r := withEnvelope(t, &Envelope{Schema: vendor})
	b, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	doc["envelope"] = json.RawMessage(`{"schema":"nas.example/transfer@2","resolve":"https://evil.invalid/s"}`)
	forged, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(forged); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}
