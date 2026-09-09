package job

import (
	"strings"
	"testing"
)

func recordWith(content, critical, spec string) []byte {
	return []byte(`{"content":[` + content + `],"critical":[` + critical +
		`],"id":"j1","kind":"download","state":"running","spec":` + spec + `}`)
}

// [JOB-D8], and RFC 9052 §3.1 behind it: a name marked critical that `content`
// does not carry is a declaration about nothing, and the reader refuses.
func TestDecodeRefusesACriticalNameContentDoesNotCarry(t *testing.T) {
	in := recordWith(`"abstraction.job/base@1"`,
		`"abstraction.job/base@1","abstraction.job/intent@1"`, `{}`)
	if _, err := Decode(in); err == nil {
		t.Fatal("critical is a subset of content; the reader accepted a name content does not carry")
	}
}

// [JOB-D10]: the never-critical names leave the list before the subset check,
// so a writer that wrongly marked an advisory feature cannot refuse a record
// this reader can act on.
func TestDecodeStripsANeverCriticalNameBeforeTheSubsetCheck(t *testing.T) {
	in := recordWith(`"abstraction.job/base@1"`,
		`"abstraction.job/base@1","abstraction.download/ranges@1"`, `{}`)
	r, err := Decode(in)
	if err != nil {
		t.Fatalf("a never-critical marking is stripped, not refused: %v", err)
	}
	out, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), FeatureRanges) {
		t.Fatal("the stripped marking came back on write")
	}
}

// [JOB-E8]: the depth limit is the record's own and an opaque value is counted
// against it, so the boundary is measured from the record object outwards.
func TestDecodeRefusesNestingPastTheDepthLimit(t *testing.T) {
	nest := func(n int) string {
		return strings.Repeat("[", n) + strings.Repeat("]", n)
	}
	base := `"abstraction.job/base@1"`
	if _, err := Decode(recordWith(base, base, nest(depthLimit-1))); err != nil {
		t.Fatalf("%d containers around the record object is the limit, not past it: %v", depthLimit, err)
	}
	if _, err := Decode(recordWith(base, base, nest(depthLimit))); err == nil {
		t.Fatalf("nesting %d deep is past a limit of %d and was accepted", depthLimit+1, depthLimit)
	}
}
