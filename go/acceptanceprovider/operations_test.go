package acceptanceprovider

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	cas "github.com/openabstractions/abstraction-cas/go"
	job "github.com/openabstractions/abstraction-job/go"
)

type resultExecutor struct {
	testExecutor
	body   []byte
	broken bool
}

func (e resultExecutor) ReadOperationResult(_ string, _ *job.Record, offset, maxBytes int64) ([]byte, int64, error) {
	if e.broken {
		return nil, 0, errors.New("private provider failure")
	}
	if offset > int64(len(e.body)) {
		return nil, 0, ErrResultRange
	}
	return e.body[offset:min(int64(len(e.body)), offset+maxBytes)], int64(len(e.body)), nil
}

func TestObserveDoesNotSealAndScopesOperations(t *testing.T) {
	p := openTest(t, t.TempDir())
	s := submission(p, "observed-before-submit")
	v, err := p.BindOperations("alice").ObserveWork(s.Identity)
	if err != nil || v.Outcome != "unknown" || v.Snapshot != nil {
		t.Fatalf("absence: %+v %v", v, err)
	}
	if _, err := os.Stat(p.requestPath("alice", s.Identity)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("observation wrote a seal")
	}
	r := accept(t, p.Bind("alice"), s)
	v, err = p.BindOperations("alice").ObserveWork(s.Identity)
	if err != nil || v.Snapshot == nil || v.Snapshot.Receipt.OperationId != r.OperationId || v.Snapshot.State != "pending" {
		t.Fatalf("observation: %+v %v", v, err)
	}
	for _, scope := range []string{"bob", ""} {
		other, _ := p.BindOperations(scope).ObserveWork(s.Identity)
		bytes, _ := p.BindOperations(scope).ReadResult(s.Identity, 0, 64)
		if other.Snapshot != nil || bytes.Chunk != nil {
			t.Fatal("operation leaked across callers")
		}
		if scope == "" && (other.Outcome != "forbidden" || bytes.Outcome != "forbidden") {
			t.Fatal("missing authority accepted")
		}
	}
	if result, _ := p.BindOperations("alice").ReadResult(s.Identity, 0, 64); result.Outcome != "unsupported" {
		t.Fatalf("admission-only: %+v", result)
	}
	if _, err := p.Bind("alice").CancelWork(s.Identity); err != nil {
		t.Fatal(err)
	}
	v, _ = p.BindOperations("alice").ObserveWork(s.Identity)
	if v.Snapshot == nil || !v.Snapshot.CancellationRequested || v.Snapshot.State != "pending" {
		t.Fatalf("intent is not terminal: %+v", v)
	}
}

func TestResultBoundsAndCancellationCompletionRace(t *testing.T) {
	e := resultExecutor{testExecutor: testExecutor{profile: "result-v1"}, body: []byte("abcdef")}
	p, err := OpenWithExecutor(t.TempDir(), "logical-owner", e)
	if err != nil {
		t.Fatal(err)
	}
	s := submission(p, "result")
	r := accept(t, p.Bind("alice"), s)
	operations := p.BindOperations("alice")
	if result, _ := operations.ReadResult(s.Identity, 0, 2); result.Outcome != "not_ready" {
		t.Fatalf("pending: %+v", result)
	}
	claim, err := p.store.Claim(r.OperationId, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if cancel, _ := p.Bind("alice").CancelWork(s.Identity); cancel.Outcome != "requested" {
		t.Fatalf("cancel: %+v", cancel)
	}
	_, err = p.store.Update(claim.ID, claim.Lease.Epoch, func(record *job.Record) error { record.State = job.StateComplete; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if cancel, _ := p.Bind("alice").CancelWork(s.Identity); cancel.Outcome != "already_terminal" {
		t.Fatalf("completed cancel: %+v", cancel)
	}
	for _, pair := range [][2]int64{{-1, 1}, {0, 0}, {0, MaxResultBytes + 1}, {7, 1}} {
		if result, _ := operations.ReadResult(s.Identity, pair[0], pair[1]); result.Outcome != "invalid" || result.Chunk != nil {
			t.Fatalf("invalid range: %+v", result)
		}
	}
	for _, pair := range [][2]int64{{0, 2}, {4, 4}, {6, 1}} {
		result, err := operations.ReadResult(s.Identity, pair[0], pair[1])
		if err != nil || result.Chunk == nil {
			t.Fatalf("read: %+v %v", result, err)
		}
		chunk := result.Chunk
		if chunk.Receipt.OperationId != r.OperationId || chunk.Offset != pair[0] || chunk.Total != 6 || chunk.Eof != (chunk.Offset+int64(len(chunk.Data)) == 6) {
			t.Fatalf("chunk: %+v", chunk)
		}
	}
	p.executor = resultExecutor{testExecutor: e.testExecutor, broken: true}
	if result, _ := operations.ReadResult(s.Identity, 0, 2); result.Outcome != "unavailable" || result.Chunk != nil {
		t.Fatalf("missing result became EOF: %+v", result)
	}
}

func TestOperationObservationRefusesOversizedAndCorruptRecords(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "oversized", true: "corrupt"}[corrupt], func(t *testing.T) {
			p := openTest(t, t.TempDir())
			s := submission(p, "bounded-observation")
			r := accept(t, p.Bind("alice"), s)
			path := filepath.Join(p.store.Root(), "jobs", r.OperationId+".json")
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data := append(original, bytes.Repeat([]byte(" "), 4*MaxSpecBytes+16385)...)
			if corrupt {
				data = []byte(`{"broken":`)
			}
			if err = os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			observed, err := p.BindOperations("alice").ObserveWork(s.Identity)
			if err != nil || observed.Outcome != "unknown" || observed.Snapshot != nil {
				t.Fatalf("bad record observed: %+v %v", observed, err)
			}
			result, err := p.BindOperations("alice").ReadResult(s.Identity, 0, 64)
			if err != nil || result.Outcome != "unknown" || result.Chunk != nil {
				t.Fatalf("bad record returned data: %+v %v", result, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(data, after) {
				t.Fatal("refusal changed record", err)
			}
			if err = os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			healthy, err := p.BindOperations("alice").ObserveWork(s.Identity)
			if err != nil || healthy.Outcome != "observed" {
				t.Fatalf("fresh observation failed: %+v %v", healthy, err)
			}
		})
	}
}

func TestMaterializeBoundsJournalUnderConditionalWrite(t *testing.T) {
	p := openTest(t, t.TempDir())
	s := submission(p, "bounded-journal")
	accept(t, p.Bind("alice"), s)
	path := p.requestPath("alice", s.Identity)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data := append(original, bytes.Repeat([]byte(" "), int(maxOperationRecordBytes)+1)...)
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = p.materialize(path); !errors.Is(err, cas.ErrTooLarge) {
		t.Fatalf("journal not bounded under edit: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, data) {
		t.Fatal("journal changed on refusal", err)
	}
}
