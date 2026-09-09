package job

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const nameBase = `{"content":["abstraction.job/base@1"],"critical":["abstraction.job/base@1"],` +
	`"id":"j1","kind":"download","state":"running","spec":SPEC,` +
	`"progress":{"done":1,"total":2,"updated_at":"2026-08-20T05:07:14.951609Z"},` +
	`"lease":{"owner":"w","epoch":2,"expires_at":"2026-08-20T05:08:14.635068Z"},` +
	`"created_at":"2026-08-20T05:07:10.967343Z","updated_at":"2026-08-20T05:07:15.134811Z"}`

// escapedDupe is [JOB-E9]'s whole point in one value: two spellings of the name
// x, which decode to the same sequence of characters and are therefore one
// member appearing twice.
const escapedDupe = "{\"x\":1,\"\\u0078\":2}"

func nameDoc(spec string) []byte {
	return []byte(strings.Replace(nameBase, "SPEC", spec, 1))
}

func TestDuplicateNameRefusedAtTopLevelOfOpaqueValue(t *testing.T) {
	if _, err := Decode(nameDoc(escapedDupe)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Decode of a spec spelling one name twice = %v, want ErrInvalid", err)
	}
}

func TestDuplicateNameRefusedThreeLevelsInsideOpaqueValue(t *testing.T) {
	deep := `{"l1":{"l2":{"l3":` + escapedDupe + `}}}`
	if _, err := Decode(nameDoc(deep)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Decode of a spec repeating a name three levels down = %v, want ErrInvalid", err)
	}
}

func TestDuplicateNameRefusedInRecordItself(t *testing.T) {
	doc := strings.Replace(string(nameDoc(`{"a":1}`)), `"kind":"download"`, `"kind":"download","kind":"archive"`, 1)
	if _, err := Decode([]byte(doc)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Decode of a record naming kind twice = %v, want ErrInvalid", err)
	}
}

// A record built in memory never passes through Decode, and a refusal reachable
// from one door and not the other is not a refusal.
func TestDuplicateNameRefusedOnValidate(t *testing.T) {
	s, _ := newTestStore(t)
	if _, err := s.Submit(Record{Kind: "some-future-kind", Spec: json.RawMessage(escapedDupe)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Submit of a spec spelling one name twice = %v, want ErrInvalid", err)
	}
	if _, err := s.Submit(Record{
		Kind:       "some-future-kind",
		Spec:       json.RawMessage(`{"a":1}`),
		Extensions: map[string]json.RawMessage{"vendor/x@1": json.RawMessage(escapedDupe)},
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Submit of an extension spelling one name twice = %v, want ErrInvalid", err)
	}
}

// [JOB-E9] is per object. Everything here repeats a name somewhere the rule does
// not reach, or spells two names that are genuinely different.
func TestNamesThatOnlyLookLikeDuplicates(t *testing.T) {
	accepted := map[string]string{
		"siblings":             `{"one":{"x":1},"two":{"x":2}}`,
		"array of objects":     `{"list":[{"x":1},{"x":2}]}`,
		"parent and child":     `{"x":{"x":1}}`,
		"case-sensitive":       `{"a":1,"A":2}`,
		"unnormalized":         "{\"\\u00e9\":1,\"e\\u0301\":2}",
		"escape is not a name": `{"a":"x","b":"\u0078"}`,
	}
	for name, spec := range accepted {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(nameDoc(spec)); err != nil {
				t.Fatalf("Decode = %v, want accepted", err)
			}
		})
	}
}

// The rule that makes [JOB-E9] and [JOB-E7] compatible: names are decoded in
// order to compare them, and nothing decoded is ever written back. A reader that
// compared correctly and then re-encoded from its own parse would pass every
// test above and still hand the next reader a different document.
func TestOpaqueBytesSurviveNameComparison(t *testing.T) {
	const spelled = `{"esc":"a\/b","o\/d":1,"\u0078":2,"ratio":1.50,` +
		`"big":12345678901234567890,"zebra":1,"apple":2}`
	r, err := Decode(nameDoc(spelled))
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Spec) != spelled {
		t.Fatalf("the reader that compared the names re-spelled the payload:\n got %s\nwant %s", r.Spec, spelled)
	}
	written, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	again, err := Decode(written)
	if err != nil {
		t.Fatal(err)
	}
	// [JOB-E1] indents the whole file, so the payload's own whitespace is the
	// record writer's to choose and its tokens are not.
	if got := unindent(string(again.Spec)); got != spelled {
		t.Fatalf("a round trip through the record writer changed the payload:\n got %s\nwant %s", got, spelled)
	}
}

func unindent(s string) string {
	var flat strings.Builder
	for _, line := range strings.Split(s, "\n") {
		flat.WriteString(strings.TrimSpace(line))
	}
	return strings.ReplaceAll(strings.ReplaceAll(flat.String(), `": `, `":`), ", ", ",")
}
