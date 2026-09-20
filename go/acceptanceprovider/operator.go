package acceptanceprovider

import (
	"errors"
	"unicode/utf8"

	job "github.com/openabstractions/abstraction-job/go"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

// boundScope is the caller scope a handler may use, or "" for none.
func boundScope(scope string) string {
	if len(scope) > MaxCallerScopeBytes || !utf8.ValidString(scope) {
		return ""
	}
	return scope
}

// methodOperator serves abstraction.job/operator@1: account-wide inventory and
// cross-scope cancellation, each decided per call by the configured method
// policy. With no configured policy every call is forbidden (JOB-A13).
type methodOperator struct {
	bound   *bound
	permit  func(string, string) access
	decided bool
}

func (h methodOperator) grant(method string) access {
	if !h.decided || h.bound.scope == "" {
		return accessDenied
	}
	return h.permit(OperatorService, method)
}

func (h methodOperator) ListAccountWork(cursor string, limit int64) (api.InventoryPage, error) {
	switch h.grant("ListAccountWork") {
	case accessAllowed:
		return h.bound.listWork(cursor, limit, true)
	case accessUnavailable:
		return inventoryRefusal(api.InventoryOutcomeUnavailable), nil
	}
	return inventoryRefusal(api.InventoryOutcomeForbidden), nil
}

func (h methodOperator) CancelOperation(operationID string) (api.OperatorCancellation, error) {
	if !validOperationID(operationID) {
		return api.OperatorCancellation{Outcome: api.OperatorCancellationOutcomeInvalid}, nil
	}
	switch h.grant("CancelOperation") {
	case accessAllowed:
		return h.bound.cancelOperation(operationID), nil
	case accessUnavailable:
		return api.OperatorCancellation{Outcome: api.OperatorCancellationOutcomeUnavailable}, nil
	}
	return api.OperatorCancellation{Outcome: api.OperatorCancellationOutcomeForbidden}, nil
}

// cancelOperation records cancellation intent on any operation of this
// provider's store, naming the caller's scope as the intent's author.
func (b *bound) cancelOperation(operationID string) api.OperatorCancellation {
	_, err := b.provider.store.SetIntent(operationID, job.WantCancel, b.scope)
	switch {
	case err == nil:
		return api.OperatorCancellation{Outcome: api.OperatorCancellationOutcomeRequested}
	case errors.Is(err, job.ErrTerminal):
		return api.OperatorCancellation{Outcome: api.OperatorCancellationOutcomeAlreadyTerminal}
	case errors.Is(err, job.ErrNotFound):
		return api.OperatorCancellation{Outcome: api.OperatorCancellationOutcomeUnknown}
	}
	return api.OperatorCancellation{Outcome: api.OperatorCancellationOutcomeUnavailable}
}
