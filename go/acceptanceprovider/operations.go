package acceptanceprovider

import (
	"encoding/json"
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

// ResultRetainer declares how long complete result bytes stay readable after
// completion [JOB-A11]. A provider without it declares no retention.
type ResultRetainer interface {
	ResultRetentionMs() int64
}

// resultRetention is the declared retention, zero when none is declared.
func (p *Provider) resultRetention() int64 {
	if _, reads := p.executor.(ResultReader); !reads {
		return 0
	}
	if r, ok := p.executor.(ResultRetainer); ok && r.ResultRetentionMs() > 0 {
		return r.ResultRetentionMs()
	}
	return 0
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
	return api.ObservationResult{Outcome: "observed", Snapshot: operationSnapshot(receipt, record, b.provider.executor, b.provider.resultLost(b.scope, id))}, nil
}

// ErrResultLost is returned by a ResultReader when a complete operation's
// result bytes no longer exist [JOB-A10]. Any other reader error is transient.
var ErrResultLost = errors.New("acceptance: operation result lost")

// resultLost reports a recorded JOB-A10 loss. The flag is irreversible, so a
// read outside the journal lock is current enough.
func (p *Provider) resultLost(scope string, id api.RequestIdentity) bool {
	j, _, err := p.readJournal(scope, id)
	return err == nil && j != nil && j.ResultLost
}

// recordResultLost durably marks a published operation's result as lost.
func (p *Provider) recordResultLost(scope string, id api.RequestIdentity) error {
	if err := p.ensureFeature(StorageFeatureResultLost); err != nil {
		return err
	}
	path := p.requestPath(scope, id)
	if err := regularFile(path); err != nil {
		return err
	}
	return cas.ChangeLimit(path, maxOperationRecordBytes, func(cur []byte) ([]byte, error) {
		j, err := p.decodeJournal(cur)
		if err != nil {
			return nil, err
		}
		if j.Phase != "published" || j.Scope != scope || j.Identity != id || j.ResultLost {
			return cur, nil
		}
		j.ResultLost = true
		return json.Marshal(j)
	})
}

func operationSnapshot(receipt *api.Receipt, record *job.Record, executor Executor, lost bool) *api.OperationSnapshot {
	snapshot := &api.OperationSnapshot{Receipt: *receipt, State: string(record.State),
		Progress:              api.WorkProgress{Done: record.Progress.Done, Total: record.Progress.Total},
		CancellationRequested: record.Intent != nil && record.Intent.Want == job.WantCancel}
	if lost {
		// A recorded loss projects the complete record as typed terminal failure.
		snapshot.State = string(job.StateFailed)
		snapshot.Failure = &api.WorkFailure{Classification: "permanent", Message: "operation result lost", Cause: "result_lost"}
		return snapshot
	}
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
	if b.provider.resultLost(b.scope, id) {
		// Reappearing bytes are never served under a lost identity [JOB-A10].
		return api.ResultRead{Outcome: "unavailable"}, nil
	}
	data, total, err := reader.ReadOperationResult(b.provider.root, record, offset, maxBytes)
	if errors.Is(err, ErrResultRange) {
		return api.ResultRead{Outcome: "invalid"}, nil
	}
	if errors.Is(err, ErrResultLost) {
		// A failed record write leaves the operation complete and the reply the
		// same; the failure goes to the provider's error reporter.
		if err := b.provider.recordResultLost(b.scope, id); err != nil {
			b.provider.reportError(errors.Join(errResultLostNotRecorded, err))
		}
		return api.ResultRead{Outcome: "unavailable"}, nil
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
