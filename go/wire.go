package job

import (
	"encoding/json"
	"errors"
	"fmt"
)

// This file is the SERVICE binding's wire format, and it exists to prove
// something the project has been claiming without evidence.
//
// The test that decides whether this is an abstraction is: the same application,
// unchanged, running on two bindings. Until now there was one — files in a
// directory — so the claim could not be checked, and the README says so.
//
// What differs here is the TRANSPORT. A client holds no directory, cannot be
// given one, and does not implement Scratch; a caller that assumed a filesystem
// finds out at compile time or through a capability check, which is the whole
// point of having made Scratch optional.
//
// The encoding is still JSON, and that is not the interesting part. Swapping it
// for something binary changes this file and nothing above it, which is exactly
// what "the encoding belongs to the binding" was supposed to mean.

// wireKind is how a sentinel error survives the crossing.
//
// Callers use errors.Is(err, ErrLeaseHeld) to decide whether to wait or give up,
// so an error that arrives as plain text has lost the only part of itself that
// mattered. Naming them is a small thing that a binding must not skip.
type wireKind string

const (
	kindNone       wireKind = ""
	kindNotFound   wireKind = "not_found"
	kindLeaseHeld  wireKind = "lease_held"
	kindStale      wireKind = "stale_epoch"
	kindExpired    wireKind = "lease_expired"
	kindTerminal   wireKind = "terminal"
	kindInvalid    wireKind = "invalid"
	kindUnknownOp  wireKind = "unknown_op"
	kindOther      wireKind = "other"
	kindNotWritten wireKind = "not_supported"
)

func kindOf(err error) wireKind {
	switch {
	case err == nil:
		return kindNone
	case errors.Is(err, ErrNotFound):
		return kindNotFound
	case errors.Is(err, ErrLeaseHeld):
		return kindLeaseHeld
	case errors.Is(err, ErrStaleEpoch):
		return kindStale
	case errors.Is(err, ErrLeaseExpiry):
		return kindExpired
	case errors.Is(err, ErrTerminal):
		return kindTerminal
	case errors.Is(err, ErrInvalid):
		return kindInvalid
	}
	return kindOther
}

func errorOf(k wireKind, text string) error {
	var base error
	switch k {
	case kindNone:
		return nil
	case kindNotFound:
		base = ErrNotFound
	case kindLeaseHeld:
		base = ErrLeaseHeld
	case kindStale:
		base = ErrStaleEpoch
	case kindExpired:
		base = ErrLeaseExpiry
	case kindTerminal:
		base = ErrTerminal
	case kindInvalid:
		base = ErrInvalid
	default:
		return errors.New(text)
	}
	return fmt.Errorf("%w: %s", base, text)
}

type request struct {
	Op     string          `json:"op"`
	ID     string          `json:"id,omitempty"`
	Owner  string          `json:"owner,omitempty"`
	Epoch  int64           `json:"epoch,omitempty"`
	TTLMS  int64           `json:"ttl_ms,omitempty"`
	Want   string          `json:"want,omitempty"`
	By     string          `json:"by,omitempty"`
	Reason string          `json:"reason,omitempty"`
	Record json.RawMessage `json:"record,omitempty"`
}

type response struct {
	Kind    wireKind          `json:"kind,omitempty"`
	Error   string            `json:"error,omitempty"`
	ID      string            `json:"id,omitempty"`
	Record  json.RawMessage   `json:"record,omitempty"`
	Records []json.RawMessage `json:"records,omitempty"`
	Bool    bool              `json:"bool,omitempty"`
}
