package job

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-job/go/rec"
)

func plain(t *testing.T, s Store) *Record {
	t.Helper()
	id, err := s.Submit(Record{Kind: "download", Spec: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A sweep that cannot read part of its store says which part, and still returns
// the rest. Zero orphans and a nil error is a supervisor calling itself healthy
// over a directory it cannot read.
func TestASweepReportsWhatItCouldNotRead(t *testing.T) {
	root := t.TempDir()
	s, err := NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	good := plain(t, s)
	if err := os.WriteFile(filepath.Join(root, "jobs", "corrupt.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		call func() ([]*Record, error)
	}{
		{"List", s.List},
		{"Orphans", s.Orphans},
	} {
		t.Run(c.name, func(t *testing.T) {
			rs, err := c.call()
			var unread *ErrUnreadable
			if !errors.As(err, &unread) {
				t.Fatalf("hid an unreadable record: err=%v", err)
			}
			if len(unread.IDs) != 1 || unread.IDs[0] != "corrupt" {
				t.Fatalf("named %v", unread.IDs)
			}
			if len(rs) != 1 || rs[0].ID != good.ID {
				t.Fatalf("returned %d records, wanted the one that decoded", len(rs))
			}
		})
	}
}

// The same, over the socket: the two answers must not collapse into one on the
// way across, or the binding has lost what the store was changed to keep.
func TestASweepReportsWhatItCouldNotReadOverTheSocket(t *testing.T) {
	root := t.TempDir()
	behind, err := NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	good := plain(t, behind)
	if err := os.WriteFile(filepath.Join(root, "jobs", "corrupt.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	remote := served(t, behind)

	rs, err := remote.Orphans()
	var unread *ErrUnreadable
	if !errors.As(err, &unread) {
		t.Fatalf("the socket hid an unreadable record: err=%v", err)
	}
	if len(unread.IDs) != 1 || unread.IDs[0] != "corrupt" {
		t.Fatalf("the socket named %v", unread.IDs)
	}
	if len(rs) != 1 || rs[0].ID != good.ID {
		t.Fatalf("the socket returned %d orphans", len(rs))
	}
}

func served(t *testing.T, behind Store) *RemoteStore {
	t.Helper()
	ln, err := listen(t)
	if err != nil {
		t.Fatal(err)
	}
	go Serve(ln, behind)
	t.Cleanup(func() { ln.Close() })
	return NewRemoteStore(ln.Addr().Network(), ln.Addr().String())
}

// The refusal a record's critical declaration asks for fires on EVERY way in and
// out of this package, not only the one that happens to use Decode. Before this,
// the disk path refused and the socket carried the same record straight through.
func TestEveryPathRefusesWhatItCannotRead(t *testing.T) {
	behind := NewMemoryStore()
	remote := served(t, behind)
	const alien = "example.com/x"
	payload := map[string]json.RawMessage{alien: json.RawMessage(`{"a":1}`)}

	t.Run("submit", func(t *testing.T) {
		_, err := remote.Submit(Record{
			Kind: "download", Spec: json.RawMessage(`{}`),
			Extensions: payload, Critical: []string{alien},
		})
		if !errors.Is(err, ErrUnknownSchema) {
			t.Fatalf("submit accepted a record it cannot read: %v", err)
		}
	})

	t.Run("load and list", func(t *testing.T) {
		r := plain(t, behind)
		behind.records[r.ID].Extensions = payload
		behind.records[r.ID].Content = append(behind.records[r.ID].Content, alien)
		behind.records[r.ID].Critical = append(behind.records[r.ID].Critical, alien)

		if _, err := remote.Load(r.ID); !errors.Is(err, ErrUnknownSchema) {
			t.Fatalf("load accepted a record it cannot read: %v", err)
		}
		rs, err := remote.List()
		var unread *ErrUnreadable
		if !errors.As(err, &unread) {
			t.Fatalf("list accepted a record it cannot read: %v", err)
		}
		if len(unread.IDs) != 1 || unread.IDs[0] != r.ID {
			t.Fatalf("list named %v, wanted %s", unread.IDs, r.ID)
		}
		if len(rs) != 0 {
			t.Fatalf("list returned %d records it cannot read", len(rs))
		}
	})

	t.Run("write", func(t *testing.T) {
		r := plain(t, behind)
		claimed, err := remote.Claim(r.ID, "w", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		_, err = remote.Update(r.ID, claimed.Lease.Epoch, func(rec *Record) error {
			rec.Extensions = payload
			rec.Critical = append(rec.Critical, alien)
			return nil
		})
		if !errors.Is(err, ErrUnknownSchema) {
			t.Fatalf("write accepted a record it cannot read: %v", err)
		}
	})
}

// The socket refuses a newer writer's unknown field for the same reason the disk
// path does — the refusal came with Decode, and it had never been on this path.
func TestTheSocketRefusesAFieldItDoesNotKnow(t *testing.T) {
	behind := NewMemoryStore()
	remote := served(t, behind)
	r := plain(t, behind)

	encoded, err := behind.records[r.ID].Encode()
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	fields["invented_by_a_newer_writer"] = json.RawMessage(`1`)
	newer, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Decode(newer); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Decode accepted an unknown field: %v", err)
	}
	if _, err := DecodeProposal(newer); !errors.Is(err, ErrInvalid) {
		t.Fatalf("DecodeProposal accepted an unknown field: %v", err)
	}
	if _, err := remote.do(wire.Request{Op: "submit", Record: wire.Raw(newer)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("the socket accepted an unknown field: %v", err)
	}
}

// An extension the reader does understand still arrives whole, with the content
// declaration that names it.
func TestExtensionsSurviveTheSocket(t *testing.T) {
	behind := NewMemoryStore()
	remote := served(t, behind)

	id, err := remote.Submit(Record{
		Kind: "download", Spec: json.RawMessage(`{}`),
		Extensions: map[string]json.RawMessage{"example.com/x": json.RawMessage(`{"a":1}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := remote.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Extensions["example.com/x"]) != `{"a":1}` {
		t.Fatalf("extension arrived as %q", got.Extensions["example.com/x"])
	}
	if !contains(got.Content, "example.com/x") {
		t.Fatalf("content arrived as %v", got.Content)
	}

	claimed, err := remote.Claim(id, "w", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	written, err := remote.Update(id, claimed.Lease.Epoch, func(rec *Record) error {
		rec.Extensions["example.com/x"] = json.RawMessage(`{"a":2}`)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(written.Extensions["example.com/x"]) != `{"a":2}` {
		t.Fatalf("a write through the socket dropped the extension: %v", written.Extensions)
	}
}
