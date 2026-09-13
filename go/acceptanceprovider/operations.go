package acceptanceprovider

import (
	"errors"
	cas "github.com/openabstractions/abstraction-cas/go"
	"os"
	"path/filepath"
	"unicode/utf8"

	job "github.com/openabstractions/abstraction-job/go"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

const MaxResultBytes int64 = 65536

var ErrResultRange = errors.New("operation result: invalid range")

// ResultReader supplies immutable completed bytes from service-owned storage.
// The provider authorizes the request before calling it and adds its receipt.
type ResultReader interface {
	ReadOperationResult(root string, record *job.Record, offset, maxBytes int64) ([]byte, int64, error)
}

// FailureReporter translates provider failure evidence into public vocabulary.
// Messages must be suitable for the caller and exclude private storage paths.
type FailureReporter interface {
	OperationFailure(*job.Record) *api.WorkFailure
}

func (p *Provider) BindOperations(scope string) api.OperationControl {
	if len(scope) > MaxCallerScopeBytes || !utf8.ValidString(scope) {
		scope = ""
	}
	return &bound{provider: p, scope: scope}
}

// operation observes existing evidence. Unknown identities stay available for
// their first submission; observing absence never creates a negative seal.
func (b *bound) operation(id api.RequestIdentity) (*api.Receipt, *job.Record, string) {
	if b.scope == "" {
		return nil, nil, "forbidden"
	}
	if id.Key == "" || id.HistoryEpoch == "" || len(id.Key) > 1024 || len(id.HistoryEpoch) > 1024 || !utf8.ValidString(id.Key) || !utf8.ValidString(id.HistoryEpoch) {
		return nil, nil, "invalid"
	}
	p := b.provider
	if id.HistoryEpoch != p.config.Epoch {
		return nil, nil, "unknown"
	}
	path := p.requestPath(b.scope, id)
	if regularFile(path) != nil {
		return nil, nil, "unknown"
	}
	data, err := cas.ReadLimit(path, maxOperationRecordBytes)
	if err != nil {
		return nil, nil, "unknown"
	}
	j, err := p.decodeJournal(data)
	if err != nil || j.Scope != b.scope || j.Identity != id {
		return nil, nil, "unknown"
	}
	if j.Phase == "sealed" {
		return nil, nil, "definitely_not_accepted"
	}
	// Finish an interrupted acceptance under the same journal lock and stable ID.
	if err := p.materialize(path); err != nil {
		return nil, nil, "unknown"
	}
	record, err := p.loadOperationRecord(j.Receipt.OperationId)
	if err != nil {
		return nil, nil, "unknown"
	}
	return j.Receipt, record, "observed"
}

func (b *bound) ObserveWork(id api.RequestIdentity) (api.ObservationResult, error) {
	receipt, record, outcome := b.operation(id)
	if outcome != "observed" {
		return api.ObservationResult{Outcome: outcome}, nil
	}
	return api.ObservationResult{Outcome: "observed", Snapshot: operationSnapshot(receipt, record, b.provider.executor)}, nil
}

func operationSnapshot(receipt *api.Receipt, record *job.Record, executor Executor) *api.OperationSnapshot {
	snapshot := &api.OperationSnapshot{Receipt: *receipt, State: string(record.State),
		Progress:              api.WorkProgress{Done: record.Progress.Done, Total: record.Progress.Total},
		CancellationRequested: record.Intent != nil && record.Intent.Want == job.WantCancel}
	if reporter, ok := executor.(FailureReporter); ok {
		snapshot.Failure = reporter.OperationFailure(record)
	} else if record.Error != "" || record.State == job.StateFailed {
		snapshot.Failure = &api.WorkFailure{Classification: "unknown", Message: "operation reported a failure"}
	}
	return snapshot
}

func (b *bound) ReadResult(id api.RequestIdentity, offset, maxBytes int64) (api.ResultRead, error) {
	if b.scope == "" {
		return api.ResultRead{Outcome: "forbidden"}, nil
	}
	if offset < 0 || maxBytes < 1 || maxBytes > MaxResultBytes {
		return api.ResultRead{Outcome: "invalid"}, nil
	}
	receipt, record, outcome := b.operation(id)
	if outcome != "observed" {
		if outcome == "definitely_not_accepted" {
			outcome = "unknown"
		}
		return api.ResultRead{Outcome: outcome}, nil
	}
	reader, ok := b.provider.executor.(ResultReader)
	if !ok {
		return api.ResultRead{Outcome: "unsupported"}, nil
	}
	switch record.State {
	case job.StateComplete:
	case job.StateFailed, job.StateCancelled:
		return api.ResultRead{Outcome: "unavailable"}, nil
	default:
		return api.ResultRead{Outcome: "not_ready"}, nil
	}
	data, total, err := reader.ReadOperationResult(b.provider.root, record, offset, maxBytes)
	if errors.Is(err, ErrResultRange) {
		return api.ResultRead{Outcome: "invalid"}, nil
	}
	if err != nil || total < 0 || offset > total || int64(len(data)) > maxBytes || int64(len(data)) > total-offset || (len(data) == 0 && offset < total) {
		return api.ResultRead{Outcome: "unavailable"}, nil
	}
	return api.ResultRead{Outcome: "data", Chunk: &api.ResultChunk{Receipt: *receipt, Offset: offset, Total: total, Data: data, Eof: offset+int64(len(data)) == total}}, nil
}

// Service observation uses the same persisted-record bound as inventory.
// Explicit native FileStore callers retain their separately selected API.
const maxOperationRecordBytes int64 = 4*MaxSpecBytes + 16384

func (p *Provider) loadOperationRecord(id string) (*job.Record, error) {
	path := filepath.Join(p.store.Root(), "jobs", id+".json")
	if err := regularFile(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, job.ErrNotFound
		}
		return nil, err
	}
	data, err := cas.ReadLimit(path, maxOperationRecordBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, job.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, job.ErrNotFound
	}
	return job.Decode(data)
}
