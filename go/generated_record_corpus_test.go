package job

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	input "github.com/openabstractions/abstraction-job/go/internal/recordinput"
	"github.com/openabstractions/abstraction-job/go/rec"
)

// The canonical cross-language corpus is private workspace input; the published
// Job repository runs its own durable-store and codec controls without it.
func privateInput(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(parts...)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			t.Skipf("%s is private-workspace input this repository does not publish; the private gate runs this check", filepath.ToSlash(path))
		}
		t.Fatal(err)
	}
	return path
}

func TestGeneratedNativeValueAdaptersAgainstExistingCorpus(t *testing.T) {
	dir := privateInput(t, "../../../idl/test/corpus")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	accepted, rejected, optionalZero, historicalTerminal := 0, 0, 0, 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			wire, err := rec.Decode(data)
			if err != nil {
				rejected++
				if native, nativeErr := Decode(data); nativeErr == nil {
					// The native reader retains pre-terminal-declaration history.
					// Grammar and unknown-critical refusals remain shared; public
					// derived-write language deliberately requires the newer name.
					var refusal *rec.Refusal
					if _, structuralErr := input.Decode(data); structuralErr != nil || !errors.As(err, &refusal) || refusal.Word != "content_mismatch" || !native.State.Terminal() || contains(native.Content, FeatureTerminal) {
						t.Fatalf("native reader accepted generated grammar/authority refusal: %v", err)
					}
					historicalTerminal++
				}
				return
			}
			accepted++
			native, err := fromGenerated(wire)
			if err != nil {
				t.Fatal(err)
			}
			projected, err := toGenerated(native)
			if err != nil {
				t.Fatal(err)
			}
			// Native Timestamp contains time.Time and represents an absent optional
			// Intent.at as its zero time. Preserve the public native writer's
			// existing explicit year-one projection; it has no presence bit.
			if wire.Intent != nil && wire.Intent.At == "" {
				wire.Intent.At = wireTime(Timestamp{})
				optionalZero++
			}
			// Compare the typed conversion independently of native semantic validation
			// and canonicalise/describe, which remain the provider's existing owners.
			expected, actual := rec.Encode(wire), rec.Encode(projected)
			if !bytes.Equal(expected, actual) {
				t.Fatalf("native-value conversion changed canonical/opaque bytes\nwant %s\ngot %s", expected, actual)
			}
		})
	}
	if accepted == 0 || rejected == 0 {
		t.Fatalf("corpus was not exercised: accepted=%d rejected=%d", accepted, rejected)
	}
	t.Logf("existing corpus: %d generated accepts, %d generated refusals, %d native optional-zero timestamp projections, %d historical terminal reads; typed conversion and structural refusal boundary checked", accepted, rejected, optionalZero, historicalTerminal)
}
