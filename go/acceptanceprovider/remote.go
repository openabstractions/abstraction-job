package acceptanceprovider

import (
	"context"
	"encoding/hex"
	"errors"

	"github.com/openabstractions/abstraction-identity/remote"
)

// RemoteConfig supplies the receiving host's local policy account and live
// exact-pin admission. Account fields never come from a certificate or frame.
type RemoteConfig struct {
	AccountKind string
	Account     string
	Admit       func(remote.Peer) error
}

// RemoteMethodPolicy applies receiving-side resource policy to the validated
// generated method. Nil denies every job method, even for an authorized peer.
type RemoteMethodPolicy func(context.Context, remote.Peer, string, string) error

// RemoteHandler serves the generated job protocols for a canonical remote
// subject. The transport owns TLS and frames; identity owns the subject of the
// currently pinned leaf key. Configure the server frame limit as MaxFrameBytes.
func (p *Provider) RemoteHandler(config RemoteConfig, policy RemoteMethodPolicy) remote.Handler {
	return func(ctx context.Context, peer remote.Peer, frame []byte) ([]byte, error) {
		if p == nil {
			return nil, errors.New("acceptance: provider required")
		}
		binding := Binding{}
		if ctx.Err() == nil {
			if subject, err := remote.SubjectOf(peer, config.Admit); err == nil {
				program := subject.Program()
				if scope, err := RemoteSubjectScope(config.AccountKind, config.Account, program); err == nil {
					selected := Binding{Scope: scope, Subject: &AuthenticatedSubject{AccountKind: config.AccountKind, Account: config.Account, Program: program}, Origin: "remote", Evidence: hex.EncodeToString(peer.Key[:])}
					if selected.valid() == nil {
						binding = selected
					}
				}
			}
		}
		permit := func(service, method, kind string) access {
			if policy == nil || binding.Scope == "" || ctx.Err() != nil {
				return accessDenied
			}
			if _, err := remote.SubjectOf(peer, config.Admit); err != nil {
				return accessDenied
			}
			granted := accessAllowed
			if err := policy(policyContext(ctx, method, kind), peer, service, method); err != nil {
				if !errors.Is(err, ErrPolicyUnavailable) {
					return accessDenied
				}
				granted = accessUnavailable
			}
			if ctx.Err() != nil {
				return accessDenied
			}
			return granted
		}
		return p.dispatchFrameBindingContext(func() context.Context { return ctx }, frame, binding, permit, policy != nil, nil)
	}
}
