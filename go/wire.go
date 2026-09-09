package job

import (
	"errors"
	"fmt"

	wire "github.com/openabstractions/abstraction-job/go/rec"
)

// This file is the SERVICE binding's wire format, and it exists to prove
// something the project has been claiming without evidence.
//
// The test that decides whether this is an abstraction is: the same
// application, unchanged, running on two bindings. Until now there was one —
// files in a directory — so the claim could not be checked, and the README says
// so.
//
// What differs here is the TRANSPORT. A client holds no directory, cannot be
// given one, and does not implement Scratch; a caller that assumed a filesystem
// finds out at compile time or through a capability check, which is the whole
// point of having made Scratch optional.
//
// The envelope itself — the two structs, their field ids, the operation names
// and the verdict words — is generated from ../job.thrift into rec/. What stays
// here is the half no notation can state: which Go sentinel error each verdict
// stands for. Callers use errors.Is(err, ErrLeaseHeld) to decide whether to wait
// or give up, so an error that arrives as plain text has lost the only part of
// itself that mattered.

func kindOf(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNotFound):
		return wire.VerdictNotFound
	case errors.Is(err, ErrLeaseHeld):
		return wire.VerdictLeaseHeld
	case errors.Is(err, ErrStaleEpoch):
		return wire.VerdictStaleEpoch
	case errors.Is(err, ErrConflict):
		return wire.VerdictConflict
	case errors.Is(err, ErrLeaseExpiry):
		return wire.VerdictLeaseExpired
	case errors.Is(err, ErrTerminal):
		return wire.VerdictTerminal
	case errors.Is(err, ErrUnknownSchema):
		return wire.VerdictUnknownSchema
	case errors.Is(err, ErrNotSupported):
		return wire.VerdictNotSupported
	case errors.Is(err, ErrInvalid):
		return wire.VerdictInvalid
	}
	return wire.VerdictOther
}

func errorOf(kind, text string) error {
	var base error
	switch kind {
	case "":
		return nil
	case wire.VerdictNotFound:
		base = ErrNotFound
	case wire.VerdictLeaseHeld:
		base = ErrLeaseHeld
	case wire.VerdictStaleEpoch:
		base = ErrStaleEpoch
	case wire.VerdictConflict:
		base = ErrConflict
	case wire.VerdictLeaseExpired:
		base = ErrLeaseExpiry
	case wire.VerdictTerminal:
		base = ErrTerminal
	case wire.VerdictInvalid:
		base = ErrInvalid
	case wire.VerdictUnknownSchema:
		base = ErrUnknownSchema
	case wire.VerdictNotSupported:
		base = ErrNotSupported
	default:
		return errors.New(text)
	}
	return fmt.Errorf("%w: %s", base, text)
}
