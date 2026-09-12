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

// HandleConnection owns one connection and serves one generated exchange after
// the shared Program identity binding. The host supplies listener lifecycle,
// authorization policy and provider configuration; this creates no daemon.
// Denied calls use the contract's forbidden responses and cannot mutate state.
func HandleConnection(ctx context.Context, conn listen.Conn, provider *Provider, authorize Authorizer) error {
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
	dispatcher := api.RecoverableAcceptanceDispatcher{Handler: provider.Bind(scope)}
	reply, err := dispatcher.ExchangeFrame(call.Frame)
	if err != nil {
		return err
	}
	return call.Reply(reply)
}
