package job

import (
	"strings"
	"testing"

	wire "github.com/openabstractions/abstraction-job/go/rec"
)

const shell = `{"content":[@CONTENT@],@CRITICAL@"id":"j1","kind":"download",` +
	`"state":"running","spec":@SPEC@,@EXTRA@` +
	`"progress":{"done":1,"total":2,"updated_at":"2026-08-20T05:07:14.951609Z"},` +
	`"lease":{"owner":"w","epoch":2,"expires_at":"2026-08-20T05:08:14.635068Z"},` +
	`"created_at":"2026-08-20T05:07:10.967343Z",` +
	`"updated_at":"2026-08-20T05:07:15.134811Z"}`

type shape struct{ content, critical, spec, extra string }

func (s shape) text() string {
	content := s.content
	if content == "" {
		content = `"abstraction.job/base@1"`
	}
	critical := s.critical
	if critical != "" {
		critical = `"critical":[` + critical + `],`
	}
	spec := s.spec
	if spec == "" {
		spec = `{}`
	}
	return strings.NewReplacer(
		"@CONTENT@", content,
		"@CRITICAL@", critical,
		"@SPEC@", spec,
		"@EXTRA@", s.extra,
	).Replace(shell)
}

// word is the refusal the generated reader says, or "" where it accepts. The
// word is the contract and the offset beside it is not, so only the word is
// returned.
func word(t *testing.T, s shape) string {
	t.Helper()
	_, err := wire.Decode([]byte(s.text()))
	if err == nil {
		return ""
	}
	r, ok := err.(*wire.Refusal)
	if !ok {
		t.Fatalf("decode failed with something that is not a refusal: %v", err)
	}
	return r.Word
}

// flat is the record with every line's indent removed and the encoder's own
// separators closed up, so what remains of a payload is the tokens its writer
// spelled. [JOB-E1] owns the whitespace; [JOB-E7] owns everything else.
func flat(b []byte) string {
	var out strings.Builder
	for _, line := range strings.Split(string(b), "\n") {
		out.WriteString(strings.TrimSpace(line))
	}
	s := strings.ReplaceAll(out.String(), `": `, `":`)
	return strings.ReplaceAll(s, ", ", ",")
}

func TestAnUnknownDelegationFieldIsRefused(t *testing.T) {
	got := word(t, shape{
		content: `"abstraction.job/base@1","abstraction.job/delegation@1"`,
		extra:   `"delegation":{"system":"s","external_id":"e","surprise":[1,2]},`,
	})
	if got != "unknown_field" {
		t.Fatalf("[JOB-F1] names delegation among the scopes that refuse; got %q", got)
	}
}

func TestAKnownDelegationStillDecodes(t *testing.T) {
	got := word(t, shape{
		content: `"abstraction.job/base@1","abstraction.job/delegation@1"`,
		extra:   `"delegation":{"system":"s","external_id":"e","delivered":true},`,
	})
	if got != "" {
		t.Fatalf("a delegation of modelled fields is a legal record; got %q", got)
	}
}

// [JOB-F2]. The value is spelled the way nothing here would spell it, so a
// reader that re-encoded rather than reproduced is visible in the comparison.
func TestAnUnknownExtensionSurvivesReadModifyWrite(t *testing.T) {
	const ext = `{"zebra":1.50,"apple":-0.0,"esc":"a\/b","big":12345678901234567890}`
	in := shape{extra: `"extensions":{"example.com/nobody-here-knows":` + ext + `},`}.text()
	v, err := wire.Decode([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	v.State = "complete"
	out := flat(wire.Encode(v))
	if !strings.Contains(out, `"example.com/nobody-here-knows":`+ext) {
		t.Fatalf("an extension this layer cannot read did not survive a change to state:\n%s", out)
	}
}

// The paired case to the one above: nothing about the payload moves when an
// unrelated envelope field does. Key order, escape spelling and number spelling
// are all its writer's, and none of them is ours to normalise [JOB-E7].
func TestOpaqueSpellingSurvivesAChangeToTheEnvelope(t *testing.T) {
	const spec = `{"zebra":1,"apple":2,"esc":"a\/b","ratio":1.50,` +
		`"zed":-0.0,"exp":1e2,"big":12345678901234567890}`
	v, err := wire.Decode([]byte(shape{spec: spec}.text()))
	if err != nil {
		t.Fatal(err)
	}
	v.State = "complete"
	out := flat(wire.Encode(v))
	if !strings.Contains(out, `"spec":`+spec) {
		t.Fatalf("the spec was re-spelled on the way through:\n%s", out)
	}
}

// The paired case to the one above, pointed the other way: the syntax is
// checked all the way down, so an escape JSON does not have refuses the record
// however deeply it is buried. \q is not one of the eight (RFC 8259 §7).
func TestAnIllegalEscapeInsideAnOpaqueObjectIsRefused(t *testing.T) {
	got := word(t, shape{spec: `{"a":{"b":[0,{"c":"x\qy"}]}}`})
	if got != "bad_string" {
		t.Fatalf("an illegal escape three levels into a spec must refuse; got %q", got)
	}
}

func TestATwiceEscapedBackslashInsideAnOpaqueObjectIsLegal(t *testing.T) {
	got := word(t, shape{spec: `{"a":{"b":[0,{"c":"x\\qy"}]}}`})
	if got != "" {
		t.Fatalf(`"x\\qy" is a backslash followed by q and is legal JSON; got %q`, got)
	}
}

func TestARawControlByteInsideAnOpaqueStringIsRefused(t *testing.T) {
	got := word(t, shape{spec: "{\"a\":\"x\ty\"}"})
	if got != "bad_string" {
		t.Fatalf("a raw tab is not legal inside a JSON string; got %q", got)
	}
}

func TestAnUnpairedSurrogateInsideAnOpaqueStringIsRefused(t *testing.T) {
	for name, spec := range map[string]string{
		"high alone": `{"a":"\ud834"}`,
		"low alone":  `{"a":"\udd1e"}`,
	} {
		if got := word(t, shape{spec: spec}); got != "bad_string" {
			t.Fatalf("%s: an unpaired surrogate escape refuses outside an opaque value and must refuse inside it; got %q", name, got)
		}
	}
	if got := word(t, shape{spec: `{"a":"𝄞"}`}); got != "" {
		t.Fatalf("a paired surrogate is one character and is legal; got %q", got)
	}
}

func TestADuplicateKeyInsideAnOpaqueObjectIsRefused(t *testing.T) {
	if got := word(t, shape{spec: `{"d":"x","d":"y"}`}); got != "duplicate_key" {
		t.Fatalf("the record refuses a repeated key and an opaque value is no different; got %q", got)
	}
	// Escaped and raw are one name, so the comparison is of what a key means and
	// not of how it was typed. Comparing spellings would let a writer smuggle
	// two values under one name past every reader that checks.
	if got := word(t, shape{spec: "{\"d\":\"x\",\"\\u0064\":\"y\"}"}); got != "duplicate_key" {
		t.Fatalf("two spellings of one key are one key; got %q", got)
	}
}

func TestABadNumberSpellingInsideAnOpaqueValueIsRefused(t *testing.T) {
	for name, spec := range map[string]string{
		"leading zero":   `{"a":01}`,
		"bare point":     `{"a":1.}`,
		"bare exponent":  `{"a":1e}`,
		"lone minus":     `{"a":-}`,
		"point no digit": `{"a":.5}`,
	} {
		got := word(t, shape{spec: spec})
		if got != "number_spelling" && got != "malformed" {
			t.Fatalf("%s: %s must refuse; got %q", name, spec, got)
		}
	}
}

// A syntactic check does not need a host number. An integer past every float64
// and past int64 is legal JSON, and a reader that converted it to validate it
// would either lose it or refuse it.
func TestAnOpaqueNumberIsNeverConvertedToAHostType(t *testing.T) {
	const spec = `{"past-float":123456789012345678901234567890,` +
		`"past-int64":9223372036854775808,"tiny":1e-400}`
	v, err := wire.Decode([]byte(shape{spec: spec}.text()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(flat(wire.Encode(v)), `"spec":`+spec) {
		t.Fatal("a number no host type holds did not survive the reader that carried it")
	}
}

// [JOB-C2]. The ranges model is advisory: a reader that knows nothing about
// `verified` resumes from the prefix and re-fetches the rest, so a writer that
// marked it critical must not be able to stop one. The marking is removed, not
// refused, and it is gone from what the reader writes back.
func TestACriticalRangesMarkingIsStrippedRatherThanRefused(t *testing.T) {
	in := shape{
		content:  `"abstraction.job/base@1"`,
		critical: `"abstraction.job/base@1","abstraction.download/ranges@1"`,
	}.text()
	v, err := wire.Decode([]byte(in))
	if err != nil {
		t.Fatalf("[JOB-C2] says strip; the reader refused: %v", err)
	}
	for _, n := range v.Critical {
		if n == "abstraction.download/ranges@1" {
			t.Fatal("the marking was carried rather than stripped")
		}
	}
	if strings.Contains(flat(wire.Encode(v)), "abstraction.download/ranges@1") {
		t.Fatal("the marking came back on write")
	}
}

// Stripping one advisory name is not permission to ignore criticality. A name
// this reader has never heard of still refuses the whole record [JOB-D1].
func TestAnUnknownCriticalNameStillRefuses(t *testing.T) {
	got := word(t, shape{
		content:  `"abstraction.job/base@1","example.com/something@1"`,
		critical: `"abstraction.job/base@1","example.com/something@1"`,
	})
	if got != "unknown_critical" {
		t.Fatalf("an unknown critical name refuses; got %q", got)
	}
}

// [JOB-I7] with [JOB-I9]: a reader too old to know the intent model must refuse
// rather than keep working on a job somebody may have asked to stop. So intent
// is markable, and marking it is not an error.
func TestIntentIsCriticalWhenPresent(t *testing.T) {
	got := word(t, shape{
		content:  `"abstraction.job/base@1","abstraction.job/intent@1"`,
		critical: `"abstraction.job/base@1","abstraction.job/intent@1"`,
		extra:    `"intent":{"want":"cancel"},`,
	})
	if got != "" {
		t.Fatalf("intent@1 is critical whenever present; got %q", got)
	}
}

func TestDelegationIsCriticalWhenPresent(t *testing.T) {
	got := word(t, shape{
		content:  `"abstraction.job/base@1","abstraction.job/delegation@1"`,
		critical: `"abstraction.job/base@1","abstraction.job/delegation@1"`,
		extra:    `"delegation":{"system":"s","external_id":"e"},`,
	})
	if got != "" {
		t.Fatalf("delegation@1 is critical whenever present; got %q", got)
	}
}

// [JOB-D8], and RFC 9052 §3.1 behind it: a label listed as critical whose
// parameter is not carried is a fatal error, not a shrug.
func TestACriticalNameOutsideContentIsNotASubset(t *testing.T) {
	got := word(t, shape{
		content:  `"abstraction.job/base@1"`,
		critical: `"abstraction.job/base@1","abstraction.job/intent@1"`,
	})
	if got != "not_a_subset" {
		t.Fatalf("critical is a subset of content; got %q", got)
	}
}

// The envelope is the one place a field this reader does not model is still
// skipped, and it is skipped without being kept. Recorded as a test rather than
// as prose because it is a live gap, not a decision: a newer peer's addition to
// a request is dropped by an older peer that answers it.
func TestTheEnvelopeAcceptsAnUnknownFieldAndDoesNotKeepIt(t *testing.T) {
	in := []byte(`{"op":"load","id":"j1","surprise":{"a":[1,2]}}`)
	q, err := wire.DecodeRequest(in)
	if err != nil {
		t.Fatalf("Request is unknown_fields = grant and must accept: %v", err)
	}
	if strings.Contains(string(wire.EncodeRequest(q)), "surprise") {
		t.Fatal("the envelope kept an unknown field; this test records that it does not")
	}
}
