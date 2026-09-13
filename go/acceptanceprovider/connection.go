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
	permit := func(service, method string) bool {
		if scope == "" || ctx.Err() != nil {
			return false
		}
		if policy != nil && policy(ctx, peer, service, method) != nil {
			return false
		}
		return ctx.Err() == nil && call.Recheck() == nil
	}
	reply, err := provider.dispatchFrame(call.Frame, scope, permit)
	if err != nil {
		return err
	}
	return call.Reply(reply)
}

func (provider *Provider) dispatchFrame(frame []byte, scope string, permit func(string, string) bool) ([]byte, error) {
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
	permit          func(string, string) bool
}

func (h methodAcceptance) handler(method string) api.RecoverableAcceptance {
	if h.permit("abstraction.job/acceptance@1", method) {
		return h.allowed
	}
	return h.denied
}
func (h methodAcceptance) GetHistoryWindow() (api.HistoryWindow, error) {
	return h.handler("GetHistoryWindow").GetHistoryWindow()
}
func (h methodAcceptance) Submit(s api.Submission) (api.AcceptanceResult, error) {
	return h.handler("Submit").Submit(s)
}
func (h methodAcceptance) Reconcile(id api.RequestIdentity) (api.AcceptanceResult, error) {
	return h.handler("Reconcile").Reconcile(id)
}
func (h methodAcceptance) CancelWork(id api.RequestIdentity) (api.CancellationResult, error) {
	return h.handler("CancelWork").CancelWork(id)
}

type methodOperations struct {
	allowed, denied api.OperationControl
	permit          func(string, string) bool
}

func (h methodOperations) handler(method string) api.OperationControl {
	if h.permit("abstraction.job/operations@1", method) {
		return h.allowed
	}
	return h.denied
}
func (h methodOperations) ObserveWork(id api.RequestIdentity) (api.ObservationResult, error) {
	return h.handler("ObserveWork").ObserveWork(id)
}
func (h methodOperations) ReadResult(id api.RequestIdentity, offset, max int64) (api.ResultRead, error) {
	return h.handler("ReadResult").ReadResult(id, offset, max)
}

type methodInventory struct {
	allowed, denied api.JobInventory
	permit          func(string, string) bool
}

func (h methodInventory) ListWork(cursor string, limit int64) (api.InventoryPage, error) {
	if h.permit("abstraction.job/inventory@1", "ListWork") {
		return h.allowed.ListWork(cursor, limit)
	}
	return h.denied.ListWork(cursor, limit)
}
