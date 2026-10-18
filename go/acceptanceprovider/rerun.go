package acceptanceprovider

import (
	"context"
	"encoding/json"
	"slices"

	cas "github.com/openabstractions/abstraction-cas/go"
	job "github.com/openabstractions/abstraction-job/go"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

// A rights refusal is no result of the work [JOB-A19]. An operation that
// failed because a rights decision refused it, at a claim, renewal,
// progress write or recheck (JOB-A18) or inside the executor's own
// decisions, is not replayed to the same request submitted again: the
// Submit is decided again, and when it is now permitted the same identity
// runs again as a new operation. Every other result, a completed one or a
// failure of the work itself, replays as before.

// RightsRefusalReporter is implemented by an executor whose own rights
// decisions can fail work, so the provider can tell such a failure from a
// failure of the work itself [JOB-A19].
type RightsRefusalReporter interface {
	// RefusedByRights reports whether the failed record ended only because
	// a rights decision refused it.
	RefusedByRights(*job.Record) bool
}

// refusedByRights reports whether record failed only for a refused rights
// decision: the provider's own standing refusal, or one its executor names.
func (p *Provider) refusedByRights(record *job.Record) bool {
	if record == nil || record.State != job.StateFailed {
		return false
	}
	if standingFailure(record) != nil {
		return true
	}
	if reporter, ok := p.executor.(RightsRefusalReporter); ok {
		return reporter.RefusedByRights(record)
	}
	return false
}

// rerunRefused runs the same request again under j's identity when j's
// operation failed for a refused rights decision and Submit was permitted
// again. The executor's admission decides again first; a refusal there is
// the answer, and nothing is written. Otherwise the journal names a new
// operation, prepared as the first one was, and the caller materializes it.
// A journal another call already moved on answers as it stands.
func (b *bound) rerunRefused(path string, j *journal, s api.Submission) (*journal, *api.AcceptanceResult) {
	p := b.provider
	if j.Origin != "" {
		// Migrated legacy work keeps its own preparation and is not run
		// again here.
		return j, nil
	}
	old := j.Receipt.OperationID
	record, err := p.loadOperationRecord(old)
	if err != nil || !p.refusedByRights(record) {
		return j, nil
	}
	// The submitter's rule is decided again, asking nobody, as JOB-A18
	// decides it; a refusal or no decision leaves the refused operation as
	// the answer and writes nothing.
	if p.standing.decide != nil && j.Subject != nil {
		refusal, err := p.standing.decide(withStandingCall(context.Background(), StandingResubmit), *j.Subject, j.Arguments.Kind)
		switch {
		case err != nil:
			unavailable := outcome(api.AcceptanceOutcomeUnavailable, "policy decision unavailable; the same identity may be presented again")
			return j, &unavailable
		case refusal != "":
			forbidden := outcome(api.AcceptanceOutcomeForbidden, refusal)
			return j, &forbidden
		}
	}
	if refused, ok := b.admission(s); ok {
		return j, &refused
	}
	operationID := job.NewID()
	var work []byte
	var requires []string
	if p.executor != nil {
		work, requires, err = p.prepareBinding(b.binding, operationID, s)
		if err != nil || len(work) > MaxSpecBytes || !json.Valid(work) {
			refused := outcome(api.AcceptanceOutcomeInvalid, "configured executor refused this work")
			return j, &refused
		}
	}
	var next *journal
	err = cas.Change(path, func(cur []byte) ([]byte, error) {
		current, err := p.decodeJournal(cur)
		if err != nil {
			return nil, err
		}
		next = current
		if current.Phase != "published" || current.Receipt == nil || current.Receipt.OperationID != old {
			return cur, nil
		}
		current.Phase = "prepared"
		current.Receipt.OperationID = operationID
		if p.executor != nil {
			current.WorkSpec, current.WorkRequires = work, slices.Clone(requires)
		}
		current.ResultLost = false
		return json.Marshal(current)
	})
	if err != nil {
		unknown := outcome(api.AcceptanceOutcomeUnknown, "owner recovery evidence unavailable")
		return j, &unknown
	}
	if err := p.indexJournal(path, next); err != nil {
		unknown := outcome(api.AcceptanceOutcomeUnknown, "owner recovery evidence unavailable")
		return j, &unknown
	}
	return next, nil
}
