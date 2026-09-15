package acceptanceprovider

import (
	"context"
	"errors"

	"github.com/openabstractions/abstraction-identity/remote"
)

// RemoteAuthorizer maps a verified remote credential to a stable service-owned
// caller namespace. Scope mapping belongs to host policy. Never copy a scope
// from the frame, certificate display name, or a claimed local process identity.
type RemoteAuthorizer func(context.Context, remote.Peer) (scope string, err error)

// RemoteMethodPolicy applies receiving-side resource policy to the validated
// generated method. Nil retains the configured authorizer's access decision.
type RemoteMethodPolicy func(context.Context, remote.Peer, string, string) error

// RemoteHandler serves the same generated acceptance, operations and inventory
// protocols as HandleConnection. The authenticated remote server owns TLS and
// frames; this handler owns caller scoping and per-method policy. Configure that
// server's frame limit as MaxFrameBytes. Work lifetime remains provider-owned.
func (p *Provider) RemoteHandler(authorize RemoteAuthorizer, policy RemoteMethodPolicy) remote.Handler {
	return func(ctx context.Context, peer remote.Peer, frame []byte) ([]byte, error) {
		if p == nil {
			return nil, errors.New("acceptance: provider required")
		}
		scope := ""
		if authorize != nil && ctx.Err() == nil {
			if selected, err := authorize(ctx, peer); err == nil {
				scope = selected
			}
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
			if ctx.Err() != nil {
				return accessDenied
			}
			return granted
		}
		return p.dispatchFrame(frame, scope, permit)
	}
}
