package acceptanceprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	cas "github.com/openabstractions/abstraction-cas/go"
	cursorcodec "github.com/openabstractions/abstraction-watch/go/cursor"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	job "github.com/openabstractions/abstraction-job/go"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	watch "github.com/openabstractions/abstraction-watch/go"
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

// WaitingReporter reads the word a kind's provider records for unfinished work
// held by a condition its submission set [JOB-A15].
type WaitingReporter interface {
	OperationWaiting(*job.Record) string
}

// waitingWord accepts 1 to 64 bytes of a-z, 0-9, _, -, . and :.
func waitingWord(word string) bool {
	if len(word) == 0 || len(word) > 64 {
		return false
	}
	for _, c := range []byte(word) {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' || c == ':') {
			return false
		}
	}
	return true
}

func (p *Provider) BindOperations(scope string) api.OperationControl {
	if len(scope) > MaxCallerScopeBytes || !utf8.ValidString(scope) {
		scope = ""
	}
	return &bound{provider: p, scope: scope}
}

// operation observes existing evidence. Unknown identities stay available for
// their first submission; observing absence never creates a negative seal.
func (b *bound) operation(id api.RequestIdentity) (*journal, *job.Record, api.ObservationOutcome) {
	if b.scope == "" {
		return nil, nil, api.ObservationOutcomeForbidden
	}
	if id.Key == "" || id.HistoryEpoch == "" || len(id.Key) > 1024 || len(id.HistoryEpoch) > 1024 || !utf8.ValidString(id.Key) || !utf8.ValidString(id.HistoryEpoch) {
		return nil, nil, api.ObservationOutcomeInvalid
	}
	p := b.provider
	if id.HistoryEpoch != p.config.Epoch {
		return nil, nil, api.ObservationOutcomeUnknown
	}
	selected, err := b.ownershipSnapshot(id)
	if err != nil {
		return nil, nil, api.ObservationOutcomeUnknown
	}
	scope := selected.scope
	path := p.requestPath(scope, id)
	if regularFile(path) != nil {
		return nil, nil, api.ObservationOutcomeUnknown
	}
	data, err := cas.ReadLimit(path, maxOperationRecordBytes)
	if err != nil {
		return nil, nil, api.ObservationOutcomeUnknown
	}
	j, err := p.decodeJournal(data)
	if err != nil || j.Scope != scope || j.Identity != id {
		return nil, nil, api.ObservationOutcomeUnknown
	}
	if j.Phase == "sealed" {
		return nil, nil, api.ObservationOutcomeDefinitelyNotAccepted
	}
	// Published admission is irreversible. Only prepared admission needs the
	// journal lock for recovery; ordinary observations validate its job below.
	if j.Phase != "published" {
		if err := p.materialize(path); err != nil {
			return nil, nil, api.ObservationOutcomeUnknown
		}
	}
	record, err := p.loadOperationRecord(j.Receipt.OperationID)
	if err != nil || validateOperationRecord(j, record) != nil {
		return nil, nil, api.ObservationOutcomeUnknown
	}
	return j, record, api.ObservationOutcomeObserved
}

func (b *bound) ObserveWork(id api.RequestIdentity) (api.ObservationResult, error) {
	accepted, record, outcome := b.operation(id)
	if outcome != api.ObservationOutcomeObserved {
		return api.ObservationResult{Outcome: outcome}, nil
	}
	return api.ObservationResult{Outcome: api.ObservationOutcomeObserved, Snapshot: operationSnapshot(accepted, record, b.provider.executor, b.provider.resultLost(accepted.Scope, id))}, nil
}

type workObservationState struct {
	once  sync.Once
	epoch string
	gate  *watch.Gate
}

func (p *Provider) workObservationSource() (string, *watch.Gate) {
	p.observation.once.Do(func() {
		instance := sha256.Sum256([]byte(job.NewID()))
		p.observation.epoch = hex.EncodeToString(instance[:16])
		p.observation.gate = watch.NewGate(watch.DefaultWaiters)
	})
	return p.observation.epoch, p.observation.gate
}

// workStamp selects exactly JOB-N1's visible record fields, excluding renewals.
func workStamp(record *job.Record) string {
	data, _ := json.Marshal([]any{record.ID, record.State, record.Progress.Done, record.Progress.Total, record.Lease.Owner, record.Error})
	stamp := sha256.Sum256(data)
	return hex.EncodeToString(stamp[:])
}

func (b *bound) FollowWork(id api.RequestIdentity, after string, waitMS int64) (api.WorkObservation, error) {
	return b.followWork(context.Background(), id, after, waitMS, func() access { return accessAllowed })
}

// followWork binds transport cancellation and its receiving authority recheck.
func (b *bound) followWork(ctx context.Context, id api.RequestIdentity, after string, waitMS int64, permit func() access) (api.WorkObservation, error) {
	refusal := func(outcome api.WorkObservationOutcome) (api.WorkObservation, error) {
		next := ""
		if outcome == api.WorkObservationOutcomeGap {
			next = after
		}
		return api.WorkObservation{Outcome: outcome, Next: next, Mechanism: api.WorkObservationMechanismPolled}, nil
	}
	if waitMS < 0 || waitMS > 30000 || len(after) > 256 || !utf8.ValidString(after) {
		return refusal(api.WorkObservationOutcomeInvalid)
	}
	if b.scope == "" {
		return refusal(api.WorkObservationOutcomeForbidden)
	}
	if id.Key == "" || id.HistoryEpoch == "" || id.Attempt < 0 || len(id.Key) > 1024 || len(id.HistoryEpoch) > 1024 || !utf8.ValidString(id.Key) || !utf8.ValidString(id.HistoryEpoch) {
		return refusal(api.WorkObservationOutcomeInvalid)
	}
	epoch, gate := b.provider.workObservationSource()
	scope := b.scope
	if b.provider.canonicalSubjects && b.binding.Subject != nil && b.binding.Origin == "local" {
		var err error
		scope, err = canonicalSubjectScope(b.binding)
		if err != nil {
			return refusal(api.WorkObservationOutcomeForbidden)
		}
	}
	boundData, _ := json.Marshal([]any{scope, id})
	boundStamp := sha256.Sum256(boundData)
	boundWord := hex.EncodeToString(boundStamp[:])
	codec := cursorcodec.Codec{Separator: ":", Scoped: true}
	if after != "" {
		envelope, ok := codec.Parse(after)
		if !ok || !envelope.Hex(16, 32, 32) {
			return refusal(api.WorkObservationOutcomeInvalid)
		}
		if envelope.Epoch != epoch {
			return refusal(api.WorkObservationOutcomeGap)
		}
		if !envelope.Bound(epoch, boundWord) {
			return refusal(api.WorkObservationOutcomeForbidden)
		}
	}
	var answer api.WorkObservation
	var subscription job.Subscription
	var cancel context.CancelFunc
	var joined chan struct{}
	defer func() {
		if subscription != nil {
			cancel()
			//unchecked: Close on the job watcher is idempotent and returns nil.
			_ = subscription.Close()
			<-joined
		}
	}()
	denied := api.WorkObservationOutcomeForbidden
	outcome, err := gate.Wait(ctx, time.Duration(waitMS)*time.Millisecond, watch.Probe{
		Read: func() (bool, error) {
			accepted, record, status := b.operation(id)
			if status != api.ObservationOutcomeObserved {
				switch status {
				case api.ObservationOutcomeForbidden:
					answer, _ = refusal(api.WorkObservationOutcomeForbidden)
				case api.ObservationOutcomeInvalid:
					answer, _ = refusal(api.WorkObservationOutcomeInvalid)
				case api.ObservationOutcomeUnavailable:
					answer, _ = refusal(api.WorkObservationOutcomeUnavailable)
				default:
					answer, _ = refusal(api.WorkObservationOutcomeUnknown)
				}
				return true, nil
			}
			next := codec.Encode(cursorcodec.Envelope{Epoch: epoch, Scope: boundWord, Payload: workStamp(record)})
			answer = api.WorkObservation{Outcome: api.WorkObservationOutcomeChanged, Next: next, Mechanism: api.WorkObservationMechanismPolled}
			if next != after {
				answer.Snapshot = operationSnapshot(accepted, record, b.provider.executor, b.provider.resultLost(accepted.Scope, id))
				return true, nil
			}
			if waitMS > 0 && subscription == nil {
				// Watch is armed inside the gate's armed read. Its first notice
				// forces another read, covering a change during attachment.
				subscription = job.Watch(b.provider.store, record.Kind)
				var watchCtx context.Context
				watchCtx, cancel = context.WithCancel(ctx)
				joined = make(chan struct{})
				go func() {
					defer close(joined)
					for {
						if _, err := subscription.Next(watchCtx); err != nil {
							return
						}
						gate.Advance()
					}
				}()
			}
			return false, nil
		},
		Recheck: func() error {
			switch permit() {
			case accessAllowed:
				return nil
			case accessUnavailable:
				denied = api.WorkObservationOutcomeUnavailable
			}
			return errors.New("acceptance: work observation authority refused")
		},
	})
	if err != nil {
		if ctx.Err() != nil {
			return api.WorkObservation{}, ctx.Err()
		}
		return refusal(denied)
	}
	switch outcome {
	case watch.Answered:
		return answer, nil
	case watch.Unchanged:
		answer.Outcome = api.WorkObservationOutcomeUnchanged
		answer.Snapshot = nil
		return answer, nil
	default:
		return refusal(api.WorkObservationOutcomeUnavailable)
	}
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

func operationSnapshot(accepted *journal, record *job.Record, executor Executor, lost bool) *api.OperationSnapshot {
	snapshot := &api.OperationSnapshot{Receipt: *accepted.Receipt, State: publicWorkState(record.State), Label: accepted.Label, LabelDerived: accepted.LabelDerived,
		Progress:              api.WorkProgress{Done: record.Progress.Done, Total: record.Progress.Total},
		CancellationRequested: record.Intent != nil && record.Intent.Want == job.WantCancel}
	if lost {
		// A recorded loss projects the complete record as typed terminal failure.
		snapshot.State = api.WorkStateFailed
		snapshot.Failure = &api.WorkFailure{Classification: api.FailureClassPermanent, Message: "operation result lost", Cause: api.FailureCauseResultLost}
		return snapshot
	}
	if reporter, ok := executor.(WaitingReporter); ok && !record.State.Terminal() {
		if word := reporter.OperationWaiting(record); waitingWord(word) {
			snapshot.Waiting = word
		}
	}
	if refused := standingFailure(record); refused != nil {
		// A rule that refused the work ended it, not the executor [JOB-A18].
		snapshot.Failure = refused
	} else if reporter, ok := executor.(FailureReporter); ok {
		snapshot.Failure = reporter.OperationFailure(record)
	} else if record.Error != "" || record.State == job.StateFailed {
		snapshot.Failure = &api.WorkFailure{Classification: api.FailureClassUnknown, Message: "operation reported a failure"}
	}
	return snapshot
}

func (b *bound) ReadResult(id api.RequestIdentity, offset, maxBytes int64) (api.ResultRead, error) {
	b.provider.resultMu.RLock()
	defer b.provider.resultMu.RUnlock()
	if b.scope == "" {
		return api.ResultRead{Outcome: api.ResultOutcomeForbidden}, nil
	}
	if offset < 0 || maxBytes < 1 || maxBytes > MaxResultBytes {
		return api.ResultRead{Outcome: api.ResultOutcomeInvalid}, nil
	}
	accepted, record, outcome := b.operation(id)
	if outcome != api.ObservationOutcomeObserved {
		return api.ResultRead{Outcome: resultOutcome(outcome)}, nil
	}
	reader, ok := b.provider.executor.(ResultReader)
	if !ok {
		return api.ResultRead{Outcome: api.ResultOutcomeUnsupported}, nil
	}
	switch record.State {
	case job.StateComplete:
	case job.StateFailed, job.StateCancelled:
		return api.ResultRead{Outcome: api.ResultOutcomeUnavailable}, nil
	default:
		return api.ResultRead{Outcome: api.ResultOutcomeNotReady}, nil
	}
	if b.provider.resultLost(accepted.Scope, id) {
		// Reappearing bytes are never served under a lost identity [JOB-A10].
		return api.ResultRead{Outcome: api.ResultOutcomeUnavailable}, nil
	}
	data, total, err := reader.ReadOperationResult(b.provider.root, record, offset, maxBytes)
	if errors.Is(err, ErrResultRange) {
		return api.ResultRead{Outcome: api.ResultOutcomeInvalid}, nil
	}
	if errors.Is(err, ErrResultLost) {
		// A failed record write leaves the operation complete and the reply the
		// same; the failure goes to the provider's error reporter.
		if err := b.provider.recordResultLost(accepted.Scope, id); err != nil {
			b.provider.reportError(errors.Join(errResultLostNotRecorded, err))
		}
		return api.ResultRead{Outcome: api.ResultOutcomeUnavailable}, nil
	}
	if err != nil || total < 0 || offset > total || int64(len(data)) > maxBytes || int64(len(data)) > total-offset || (len(data) == 0 && offset < total) {
		return api.ResultRead{Outcome: api.ResultOutcomeUnavailable}, nil
	}
	return api.ResultRead{Outcome: api.ResultOutcomeData, Chunk: &api.ResultChunk{Receipt: *accepted.Receipt, Offset: offset, Total: total, Data: data, EOF: offset+int64(len(data)) == total}}, nil
}

func publicWorkState(state job.State) api.WorkState {
	switch state {
	case job.StatePending:
		return api.WorkStatePending
	case job.StateRunning:
		return api.WorkStateRunning
	case job.StateTransferred:
		return api.WorkStateTransferred
	case job.StateComplete:
		return api.WorkStateComplete
	case job.StateFailed:
		return api.WorkStateFailed
	case job.StateCancelled:
		return api.WorkStateCancelled
	default:
		return 0
	}
}

func resultOutcome(outcome api.ObservationOutcome) api.ResultOutcome {
	switch outcome {
	case api.ObservationOutcomeForbidden:
		return api.ResultOutcomeForbidden
	case api.ObservationOutcomeInvalid:
		return api.ResultOutcomeInvalid
	case api.ObservationOutcomeUnavailable:
		return api.ResultOutcomeUnavailable
	case api.ObservationOutcomeUnknown, api.ObservationOutcomeDefinitelyNotAccepted:
		return api.ResultOutcomeUnknown
	default:
		return 0
	}
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
