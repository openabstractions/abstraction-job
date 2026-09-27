package job

import (
	"errors"
	"testing"
)

// A reader takes the next release's spelling of not_supported (vocabulary N24)
// alongside the current one, and every Verdict it writes reads back as itself.
func TestErrorOfReadsBothUnsupportedSpellings(t *testing.T) {
	for _, kind := range []string{"not_supported", "unsupported"} {
		if err := errorOf(kind, "no such action"); !errors.Is(err, ErrNotSupported) {
			t.Errorf("errorOf(%q) = %v, want ErrNotSupported", kind, err)
		}
	}
	for _, want := range []error{ErrNotFound, ErrLeaseHeld, ErrStaleEpoch, ErrConflict, ErrLeaseExpiry, ErrTerminal, ErrInvalid, ErrUnknownSchema, ErrNotSupported} {
		if got := errorOf(Verdict(want), "x"); !errors.Is(got, want) {
			t.Errorf("errorOf(Verdict(%v)) = %v", want, got)
		}
	}
}
