package job

import (
	"encoding/json"
	"strings"
	"testing"
)

// A spec whose every scalar is spelled a way this package would not choose:
// a redundant solidus escape in a value and in a key, a trailing zero, an
// exponent, a negative zero, an integer past float64, and members in an order
// nothing would sort them into.
const hostileSpec = `{"esc":"a\/b","o\/d":1,"ratio":1.50,"exp":1e2,` +
	`"zed":-0.0,"big":12345678901234567890,"zebra":1,"apple":2}`

// TestOpaqueBytesSurvive is [JOB-E7]. The bytes of a spec belong to whoever
// wrote them, and a store that returns 1.5 for the 1.50 it was handed has
// rewritten somebody else's document on their behalf.
func TestOpaqueBytesSurvive(t *testing.T) {
	s, _ := newTestStore(t)
	id, err := s.Submit(Record{Kind: "some-future-kind", Spec: json.RawMessage(hostileSpec)})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	written, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	// [JOB-E1] indents the whole file, so the payload's own whitespace is the
	// record writer's to choose and its tokens are not.
	var flat strings.Builder
	for _, line := range strings.Split(string(written), "\n") {
		flat.WriteString(strings.TrimSpace(line))
	}
	compact := strings.ReplaceAll(strings.ReplaceAll(flat.String(), `": `, `":`), ", ", ",")
	if !strings.Contains(compact, hostileSpec) {
		t.Fatalf("the spec was re-spelled on the way through:\n%s", written)
	}
}
