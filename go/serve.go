package job

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"time"

	wire "github.com/openabstractions/abstraction-job/go/rec"
)

// Serve exposes a store over a connection, so something else can be its client.
//
// This is the far half of the service binding. It wraps any Store — today a
// FileStore, tomorrow whatever is underneath a supervisor — and the client on
// the other end cannot tell which, which is the property being demonstrated.
//
// One request per connection. Not for elegance: a connection that carries one
// exchange and closes cannot leave a half-read reply in a buffer for the next
// caller to misparse, and the cost is a socket setup on a local pipe. When this
// becomes the deployed path rather than the proof, that is the first thing to
// revisit.
func Serve(ln net.Listener, store Store) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(30 * time.Second))
			serveOne(conn, store)
		}()
	}
}

func serveOne(conn net.Conn, store Store) {
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	req, err := wire.DecodeRequest(line)
	if err != nil {
		reply(conn, wire.Response{Kind: wire.VerdictInvalid, Error: err.Error()})
		return
	}
	reply(conn, apply(store, *req))
}

func apply(store Store, req wire.Request) wire.Response {
	switch req.Op {
	case "submit":
		r, err := DecodeProposal([]byte(req.Record))
		if err != nil {
			return fail(err)
		}
		id, err := store.Submit(*r)
		if err != nil {
			return fail(err)
		}
		return wire.Response{Id: id}

	case "load":
		r, err := store.Load(req.Id)
		return one(r, err)

	case "list":
		rs, err := store.List()
		return many(rs, err)

	case "orphans":
		rs, err := store.Orphans()
		return many(rs, err)

	case "claimable":
		r, err := store.Load(req.Id)
		if err != nil {
			return fail(err)
		}
		return wire.Response{Bool: store.Claimable(r)}

	case "claim":
		r, err := store.Claim(req.Id, req.Owner, time.Duration(req.TtlMs)*time.Millisecond)
		return one(r, err)

	case "renew":
		r, err := store.Renew(req.Id, req.Epoch, time.Duration(req.TtlMs)*time.Millisecond)
		return one(r, err)

	case "release":
		if err := store.Release(req.Id, req.Epoch); err != nil {
			return fail(err)
		}
		return wire.Response{}

	case "set_intent":
		r, err := store.SetIntent(req.Id, Want(req.Want), req.By)
		return one(r, err)

	case "recall":
		r, err := store.Recall(req.Id, req.Epoch, req.Reason, req.By, time.Duration(req.TtlMs)*time.Millisecond)
		return one(r, err)

	case "write":
		want, err := Decode([]byte(req.Record))
		if err != nil {
			return fail(err)
		}
		if len(req.Base) == 0 {
			return fail(fmt.Errorf("%w: a write must present the record it was computed from", ErrInvalid))
		}
		r, err := store.Update(req.Id, req.Epoch, func(rec *Record) error {
			// The epoch is not enough here, and believing it was is what this
			// check repairs. An epoch does not move when its holder writes, so
			// two writers under ONE lease — a reporter and a checkpointer in the
			// same process — both present a valid epoch, and the second
			// whole-record write restores the fields it did not touch to what
			// they were when it read. The file and memory bindings cannot lose a
			// write that way because they run the caller's closure against the
			// record as it stands; a closure cannot cross the wire, so the same
			// condition is expressed as the bytes the caller read.
			same, err := unchanged(rec, req.Base)
			if err != nil {
				return err
			}
			if !same {
				return fmt.Errorf("%w: %s", ErrConflict, req.Id)
			}
			// [JOB-V4], said out loud rather than arranged. Leaving the envelope
			// out of the enumeration below would have been enough to stop it
			// moving — and it would have made this binding SILENTLY IGNORE a
			// change the other two REFUSE, which is the same application on two
			// bindings behaving differently, the one test that decides whether
			// this is an abstraction. A guarantee held by the absence of a line
			// is a guarantee nothing can fail.
			if err := envelopeUnmoved(rec.Envelope, want.Envelope); err != nil {
				return err
			}
			// Everything a lease holder is allowed to change. Deliberately
			// enumerated: id, kind, spec, the envelope and the timestamps are
			// not the caller's to move, and a wire format that let them would be
			// a way to rewrite history through a socket.
			rec.State = want.State
			rec.Progress = want.Progress
			rec.Checkpoint = want.Checkpoint
			rec.Delegation = want.Delegation
			rec.Error = want.Error
			// Dropping an extension here would silently destroy a participant's
			// data — see Record.Extensions rule 1 — and the in-process binding
			// lets a lease holder write one, so this one must too or the same
			// application behaves differently on two bindings.
			rec.Extensions = want.Extensions
			return nil
		})
		return one(r, err)
	}
	return wire.Response{Kind: wire.UnknownOperation, Error: "unknown op " + req.Op}
}

// unchanged reports whether the store still holds the record a caller says it
// read.
//
// Both sides go through Encode rather than comparing the bytes as they arrived,
// because the bytes as they arrived are never the bytes the store holds: this
// layer writes a record indented and the transport compacts it on the way out.
// Comparing the record's own canonical form also means a client may rebuild its
// base from the record it decoded rather than having to keep the exact buffer,
// which is what makes this implementable by a client that is not this one.
func unchanged(current *Record, base wire.Raw) (bool, error) {
	asRead, err := Decode([]byte(base))
	if err != nil {
		return false, err
	}
	held, err := current.Encode()
	if err != nil {
		return false, err
	}
	seen, err := asRead.Encode()
	if err != nil {
		return false, err
	}
	return bytes.Equal(held, seen), nil
}

func fail(err error) wire.Response {
	return wire.Response{Kind: kindOf(err), Error: err.Error()}
}

func one(r *Record, err error) wire.Response {
	if err != nil {
		return fail(err)
	}
	b, err := r.Encode()
	if err != nil {
		return fail(err)
	}
	return wire.Response{Record: wire.Raw(b)}
}

// many carries the sweep's two answers separately. Collapsing an ErrUnreadable
// into fail() would hand the client an empty list and an error, which is the
// binding losing the distinction the store was changed to keep.
func many(rs []*Record, err error) wire.Response {
	var found *ErrUnreadable
	if err != nil && !errors.As(err, &found) {
		return fail(err)
	}
	unread := ErrUnreadable{}
	if found != nil {
		unread = *found
	}
	out := make([]wire.Raw, 0, len(rs))
	for _, r := range rs {
		b, err := r.Encode()
		if err != nil {
			// One record this server cannot put on the wire is one record
			// missing, not a sweep that failed. Encode re-checks what Decode
			// let through — a checkpoint valid as JSON and unreadable as ranges
			// is the case — so this is reachable, and losing the other ninety-nine
			// jobs to it would be the silent-zero defect wearing the other face.
			unread.note(r.ID, err)
			continue
		}
		out = append(out, wire.Raw(b))
	}
	resp := wire.Response{Records: out}
	if len(unread.IDs) > 0 {
		resp.Unreadable, resp.Error = unread.IDs, unread.Reason.Error()
	}
	return resp
}

func reply(conn net.Conn, resp wire.Response) {
	conn.Write(append(wire.EncodeResponse(&resp), '\n'))
}
