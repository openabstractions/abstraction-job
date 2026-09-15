package acceptanceprovider

import (
	"context"
	"errors"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

// MaxFrameBytes includes base64 expansion of the bounded binary specification.
const MaxFrameBytes uint32 = 2 * MaxSpecBytes

// Authorizer derives a stable scope from receiving-side peer evidence and host
// policy. A process ID is not a restart-stable caller scope. Errors deny access;
// a nonempty scope returned alongside an error is never used.
type Authorizer func(*identity.Peer) (scope string, err error)

// MethodPolicy is a service-owned decision for a validated generated method.
// It receives bound caller evidence, never the runtime's ambient identity.
// Errors refuse the call; implementations must honor context cancellation.
// Nil retains the host Authorizer's explicitly configured access policy.
type MethodPolicy func(context.Context, *identity.Peer, string, string) error

// ErrPolicyUnavailable marks a MethodPolicy error meaning the decision could not
// be obtained. Submit, Reconcile, CancelWork, ObserveWork, ReadResult and
// ListWork then return unavailable with no state read or change [JOB-A9].
// GetHistoryWindow refuses. Any other error is an evaluated refusal.
var ErrPolicyUnavailable = errors.New("acceptance: policy decision unavailable")

type access int

const (
	accessDenied access = iota
	accessAllowed
	accessUnavailable
)

// HandleConnection owns one connection and serves one generated exchange after
// the shared Program identity binding. The host supplies listener lifecycle,
// authorization policy and provider configuration; this creates no daemon.
// Denied calls use the contract's forbidden responses and cannot mutate state.
func HandleConnection(ctx context.Context, conn listen.Conn, provider *Provider, authorize Authorizer) error {
	return HandleConnectionWithPolicy(ctx, conn, provider, authorize, nil)
}

// HandleConnectionWithPolicy adds method-level enforcement without changing the
// caller namespace. The generated dispatcher validates arguments before policy.
func HandleConnectionWithPolicy(ctx context.Context, conn listen.Conn, provider *Provider, authorize Authorizer, policy MethodPolicy) error {
	if provider == nil {
		_ = conn.Close()
		return errors.New("acceptance: provider required")
	}
	call, err := listen.ReceiveFramed(ctx, conn, listen.Program, MaxFrameBytes)
	if err != nil {
		return err
	}
	defer call.Close()
	peer, err := call.Peer()
	if err != nil {
		return err
	}
	scope := ""
	if authorize != nil {
		if allowedScope, err := authorize(peer); err == nil {
			scope = allowedScope
		}
	}
	// Recheck after host policy, before any admission/seal or cancellation mutation.
	if err := call.Recheck(); err != nil {
		return err
	}
	permit := func(service, method string) access {
		if scope == "" || ctx.Err() != nil {
			return accessDenied
		}
		granted := accessAllowed
		if policy != nil {
			if err := policy(ctx, peer, service, method); err != nil {
				if !errors.Is(err, ErrPolicyUnavailable) {
					return accessDenied
				}
				granted = accessUnavailable
			}
		}
		if ctx.Err() != nil || call.Recheck() != nil {
			return accessDenied
		}
		return granted
	}
	reply, err := provider.dispatchFrame(call.Frame, scope, permit)
	if err != nil {
		return err
	}
	return call.Reply(reply)
}

func (provider *Provider) dispatchFrame(frame []byte, scope string, permit func(string, string) access) ([]byte, error) {
	service, err := api.ServiceName(frame)
	if err != nil {
		return nil, err
	}
	var reply []byte
	if service == "abstraction.job/inventory@1" {
		dispatcher := api.JobInventoryDispatcher{Handler: methodInventory{allowed: provider.BindInventory(scope), denied: provider.BindInventory(""), permit: permit}}
		reply, err = dispatcher.ExchangeFrame(frame)
	} else if service == "abstraction.job/operations@1" {
		dispatcher := api.OperationControlDispatcher{Handler: methodOperations{allowed: provider.BindOperations(scope), denied: provider.BindOperations(""), permit: permit}}
		reply, err = dispatcher.ExchangeFrame(frame)
	} else {
		dispatcher := api.RecoverableAcceptanceDispatcher{Handler: methodAcceptance{allowed: provider.Bind(scope), denied: provider.Bind(""), permit: permit}}
		reply, err = dispatcher.ExchangeFrame(frame)
	}
	return reply, err
}

type methodAcceptance struct {
	allowed, denied api.RecoverableAcceptance
	permit          func(string, string) access
}

func (h methodAcceptance) handler(method string) (api.RecoverableAcceptance, access) {
	granted := h.permit("abstraction.job/acceptance@1", method)
	if granted == accessAllowed {
		return h.allowed, granted
	}
	return h.denied, granted
}

const policyUnavailableReason = "policy decision unavailable; the same identity may be presented again"

func (h methodAcceptance) GetHistoryWindow() (api.HistoryWindow, error) {
	handler, _ := h.handler("GetHistoryWindow")
	return handler.GetHistoryWindow()
}
func (h methodAcceptance) Submit(s api.Submission) (api.AcceptanceResult, error) {
	handler, granted := h.handler("Submit")
	if granted == accessUnavailable {
		return outcome("unavailable", policyUnavailableReason), nil
	}
	return handler.Submit(s)
}
func (h methodAcceptance) Reconcile(id api.RequestIdentity) (api.AcceptanceResult, error) {
	handler, granted := h.handler("Reconcile")
	if granted == accessUnavailable {
		return outcome("unavailable", policyUnavailableReason), nil
	}
	return handler.Reconcile(id)
}
func (h methodAcceptance) CancelWork(id api.RequestIdentity) (api.CancellationResult, error) {
	handler, granted := h.handler("CancelWork")
	if granted == accessUnavailable {
		return api.CancellationResult{Outcome: "unavailable"}, nil
	}
	return handler.CancelWork(id)
}

type methodOperations struct {
	allowed, denied api.OperationControl
	permit          func(string, string) access
}

func (h methodOperations) ObserveWork(id api.RequestIdentity) (api.ObservationResult, error) {
	switch h.permit("abstraction.job/operations@1", "ObserveWork") {
	case accessAllowed:
		return h.allowed.ObserveWork(id)
	case accessUnavailable:
		return api.ObservationResult{Outcome: "unavailable"}, nil
	}
	return h.denied.ObserveWork(id)
}
func (h methodOperations) ReadResult(id api.RequestIdentity, offset, max int64) (api.ResultRead, error) {
	switch h.permit("abstraction.job/operations@1", "ReadResult") {
	case accessAllowed:
		return h.allowed.ReadResult(id, offset, max)
	case accessUnavailable:
		return api.ResultRead{Outcome: "unavailable"}, nil
	}
	return h.denied.ReadResult(id, offset, max)
}

type methodInventory struct {
	allowed, denied api.JobInventory
	permit          func(string, string) access
}

func (h methodInventory) ListWork(cursor string, limit int64) (api.InventoryPage, error) {
	switch h.permit("abstraction.job/inventory@1", "ListWork") {
	case accessAllowed:
		return h.allowed.ListWork(cursor, limit)
	case accessUnavailable:
		return inventoryRefusal("unavailable"), nil
	}
	return h.denied.ListWork(cursor, limit)
}
