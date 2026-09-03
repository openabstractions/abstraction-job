package job

import (
	"bufio"
	"encoding/json"
	"net"
	"time"
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
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		reply(conn, response{Kind: kindInvalid, Error: err.Error()})
		return
	}
	reply(conn, apply(store, req))
}

func apply(store Store, req request) response {
	switch req.Op {
	case "submit":
		var r Record
		if err := json.Unmarshal(req.Record, &r); err != nil {
			return response{Kind: kindInvalid, Error: err.Error()}
		}
		id, err := store.Submit(r)
		if err != nil {
			return fail(err)
		}
		return response{ID: id}

	case "load":
		r, err := store.Load(req.ID)
		return one(r, err)

	case "list":
		rs, err := store.List()
		return many(rs, err)

	case "orphans":
		rs, err := store.Orphans()
		return many(rs, err)

	case "claimable":
		r, err := store.Load(req.ID)
		if err != nil {
			return fail(err)
		}
		return response{Bool: store.Claimable(r)}

	case "claim":
		r, err := store.Claim(req.ID, req.Owner, time.Duration(req.TTLMS)*time.Millisecond)
		return one(r, err)

	case "renew":
		r, err := store.Renew(req.ID, req.Epoch, time.Duration(req.TTLMS)*time.Millisecond)
		return one(r, err)

	case "release":
		if err := store.Release(req.ID, req.Epoch); err != nil {
			return fail(err)
		}
		return response{}

	case "set_intent":
		r, err := store.SetIntent(req.ID, Want(req.Want), req.By)
		return one(r, err)

	case "write":
		// The closure form of Update cannot cross a process boundary, which is
		// the thing writing the IDL caught. A client therefore reads, applies
		// its mutation locally, and sends the RESULT — and the epoch it presents
		// is what makes that safe: the server refuses it if the record moved
		// underneath, exactly as it would refuse a stale local write.
		//
		// It is not identical to running the closure here. A caller whose
		// mutation depends on state that changed between its read and this write
		// gets its epoch rejected rather than silently applying to the wrong
		// record, which is the failure mode worth having.
		var want Record
		if err := json.Unmarshal(req.Record, &want); err != nil {
			return response{Kind: kindInvalid, Error: err.Error()}
		}
		r, err := store.Update(req.ID, req.Epoch, func(rec *Record) error {
			// Everything a lease holder is allowed to change. Deliberately
			// enumerated: id, kind, spec and the timestamps are not the caller's
			// to move, and a wire format that let them would be a way to rewrite
			// history through a socket.
			rec.State = want.State
			rec.Progress = want.Progress
			rec.Checkpoint = want.Checkpoint
			rec.Delegation = want.Delegation
			rec.Error = want.Error
			return nil
		})
		return one(r, err)
	}
	return response{Kind: kindUnknownOp, Error: "unknown op " + req.Op}
}

func fail(err error) response {
	return response{Kind: kindOf(err), Error: err.Error()}
}

func one(r *Record, err error) response {
	if err != nil {
		return fail(err)
	}
	b, err := json.Marshal(r)
	if err != nil {
		return fail(err)
	}
	return response{Record: b}
}

func many(rs []*Record, err error) response {
	if err != nil {
		return fail(err)
	}
	out := make([]json.RawMessage, 0, len(rs))
	for _, r := range rs {
		b, err := json.Marshal(r)
		if err != nil {
			return fail(err)
		}
		out = append(out, b)
	}
	return response{Records: out}
}

func reply(conn net.Conn, resp response) {
	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	conn.Write(append(b, '\n'))
}
