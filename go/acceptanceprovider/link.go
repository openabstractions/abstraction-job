package acceptanceprovider

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"unicode/utf8"

	identity "github.com/openabstractions/abstraction-identity"
	job "github.com/openabstractions/abstraction-job/go"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

// MaxLinkDestinationBytes bounds a LinkResult destination [JOB-A17].
const MaxLinkDestinationBytes = 4096

// ResultNamer names the file that holds a complete operation's result, as a
// local path under the provider root: the same bytes its ResultReader reads.
// An executor without it cannot link a result [JOB-A17].
type ResultNamer interface {
	OperationResultName(record *job.Record) (string, error)
}

// ErrResultNotNamed is a ResultNamer's answer for a kind whose result is no
// file to link, such as one composed on read; LinkResult is then
// unsupported.
var ErrResultNotNamed = errors.New("acceptance: this kind's result is no file to link")

// LinkAuthority decides whether the local peer may create destination by its
// own authority [JOB-A17]. A nil error permits the link. The provider's
// default is SameAuthority.
type LinkAuthority func(peer *identity.Peer, destination string) error

// SetLinkAuthority replaces the provider's LinkAuthority; nil restores
// SameAuthority.
func (p *Provider) SetLinkAuthority(authority LinkAuthority) {
	p.linkAuthority = authority
}

// linkFile makes the hard link, statPath, lstatPath and holdFinal read the
// destination; tests replace them to see which run.
var (
	linkFile  = os.Link
	statPath  = os.Stat
	lstatPath = os.Lstat
	holdFinal = holdFinalPath
)

// principal is an account and, on Windows, its mandatory integrity RID and
// whether its token is restricted.
type principal struct {
	account    string
	integrity  uint32
	restricted bool
}

// errNotCallerAuthority refuses a link the caller could not make itself.
var errNotCallerAuthority = errors.New("acceptance: the caller cannot create this path by its own authority")

// selfUser is the provider process's own principal (identity ID-S1); tests
// replace it.
var selfUser = identity.Self

// SameAuthority permits a link only for a caller that runs as the provider's
// own account at no lower integrity, with an unrestricted token: whatever the
// provider may create there, that caller may create itself, so the provider
// adds no authority of its own.
func SameAuthority(peer *identity.Peer, _ string) error {
	if peer == nil {
		return errNotCallerAuthority
	}
	user, err := peer.User.AtLeast(identity.ProofKernel)
	if err != nil {
		return errNotCallerAuthority
	}
	return sameAuthorityAs(user)
}

// sameAuthorityAs compares a caller's kernel-proven User with the provider's
// own. A provider whose own integrity level is unknown links for no one.
func sameAuthorityAs(user identity.User) error {
	ownUser, err := selfUser()
	if err != nil {
		return errNotCallerAuthority
	}
	own, err := userPrincipal(ownUser)
	if err != nil {
		return errNotCallerAuthority
	}
	caller, err := userPrincipal(user)
	if err != nil {
		return err
	}
	return sameOrLowerAuthority(caller, own)
}

// userPrincipal is a User's account and, on Windows, its integrity RID and
// restricted flag. A Windows User whose integrity level was not read has no
// principal: RID 0 there would read as the lowest level.
func userPrincipal(user identity.User) (principal, error) {
	if user.Kind == "windows" {
		if user.SID == "" || user.Integrity == "" || user.Integrity == "unknown" {
			return principal{}, errNotCallerAuthority
		}
		return principal{account: user.SID, integrity: user.IntegrityRID, restricted: user.Restricted}, nil
	}
	if user.UID < 0 {
		return principal{}, errNotCallerAuthority
	}
	return principal{account: strconv.Itoa(user.UID)}, nil
}

// sameOrLowerAuthority refuses a caller of another account, of a lower
// integrity than the provider, or holding a restricted token, whose
// restricting SIDs may deny writes the account itself may make.
func sameOrLowerAuthority(caller, own principal) error {
	if caller.account == "" || caller.account != own.account || caller.integrity < own.integrity || caller.restricted {
		return errNotCallerAuthority
	}
	return nil
}

// BindOperationsBinding binds operation control to a receiver-selected
// binding. LinkResult needs its local peer; the other methods read its scope.
func (p *Provider) BindOperationsBinding(binding Binding) api.OperationControl {
	if binding.valid() != nil {
		binding = Binding{}
	}
	return &bound{provider: p, scope: binding.Scope, binding: binding}
}

func linkOutcome(outcome api.ObservationOutcome) api.LinkOutcome {
	switch outcome {
	case api.ObservationOutcomeForbidden:
		return api.LinkOutcomeForbidden
	case api.ObservationOutcomeInvalid:
		return api.LinkOutcomeInvalid
	case api.ObservationOutcomeUnavailable:
		return api.LinkOutcomeUnavailable
	default:
		return api.LinkOutcomeUnknown
	}
}

// LinkResult gives a complete result a second name at destination by hard
// link [JOB-A17]. It never copies, never seals an absent identity, and
// leaves the provider's own name and retention as they were.
func (b *bound) LinkResult(id api.RequestIdentity, destination string) (api.ResultLink, error) {
	refuse := func(outcome api.LinkOutcome) (api.ResultLink, error) {
		return api.ResultLink{Outcome: outcome}, nil
	}
	if b.scope == "" {
		return refuse(api.LinkOutcomeForbidden)
	}
	accepted, record, outcome := b.operation(id)
	if outcome != api.ObservationOutcomeObserved {
		return refuse(linkOutcome(outcome))
	}
	// Only a caller the receiver proved on this machine may name a path
	// here; a remote runtime's mapping keeps reading with ReadResult.
	if b.binding.Origin != "local" || b.binding.Peer == nil {
		return refuse(api.LinkOutcomeRemote)
	}
	p := b.provider
	if len(destination) == 0 || len(destination) > MaxLinkDestinationBytes || !utf8.ValidString(destination) ||
		!filepath.IsAbs(destination) || filepath.Clean(destination) != destination {
		return refuse(api.LinkOutcomeInvalid)
	}
	namer, names := p.executor.(ResultNamer)
	if _, reads := p.executor.(ResultReader); !names || !reads {
		return refuse(api.LinkOutcomeUnsupported)
	}
	switch record.State {
	case job.StateComplete:
	case job.StateFailed, job.StateCancelled:
		return refuse(api.LinkOutcomeUnavailable)
	default:
		return refuse(api.LinkOutcomeNotReady)
	}
	if p.resultLost(b.scope, id) {
		return refuse(api.LinkOutcomeUnavailable)
	}
	name, err := namer.OperationResultName(record)
	if errors.Is(err, ErrResultNotNamed) {
		return refuse(api.LinkOutcomeUnsupported)
	}
	if err != nil || !filepath.IsLocal(name) {
		return refuse(api.LinkOutcomeUnavailable)
	}
	area, err := os.OpenRoot(p.root)
	if err != nil {
		return refuse(api.LinkOutcomeUnavailable)
	}
	info, err := area.Lstat(name)
	//unchecked: the root handle only bounded the lookup above
	_ = area.Close()
	if errors.Is(err, os.ErrNotExist) {
		// Absence of a complete operation's bytes is definitive [JOB-A10].
		if err := p.recordResultLost(b.scope, id); err != nil {
			p.reportError(errors.Join(errResultLostNotRecorded, err))
		}
		return refuse(api.LinkOutcomeUnavailable)
	}
	if err != nil || !info.Mode().IsRegular() {
		return refuse(api.LinkOutcomeUnavailable)
	}
	authority := p.linkAuthority
	if authority == nil {
		authority = SameAuthority
	}
	// Nothing on the destination is read or opened before the caller's own
	// authority to create it is established.
	if authority(b.binding.Peer, destination) != nil {
		return refuse(api.LinkOutcomeForbidden)
	}
	// A destination off a local drive, such as a share, is never opened: a
	// hard link cannot reach it, and an open would send the provider's
	// credentials to that host (identity.LocalPath).
	if identity.LocalPath(destination) != nil {
		return refuse(api.LinkOutcomeOtherVolume)
	}
	parent, err := statPath(filepath.Dir(destination))
	if err != nil || !parent.IsDir() {
		return refuse(api.LinkOutcomeInvalid)
	}
	// The parent's final path, through every junction and link, decides by
	// file identity: a parent that is the provider root or lies inside it,
	// also through a junction, names the provider's own layout.
	realParent, release, err := holdFinal(filepath.Dir(destination))
	if err != nil {
		return refuse(api.LinkOutcomeInvalid)
	}
	defer release()
	realRoot, err := finalPath(p.root)
	if err != nil {
		return refuse(api.LinkOutcomeUnavailable)
	}
	if same, err := identity.SameFile(realParent, realRoot); err != nil || same {
		return refuse(api.LinkOutcomeInvalid)
	}
	if inside, err := identity.Within(realParent, realRoot); err != nil || inside {
		return refuse(api.LinkOutcomeInvalid)
	}
	destination = filepath.Join(realParent, filepath.Base(destination))
	if _, err := lstatPath(destination); err == nil {
		return refuse(api.LinkOutcomeExists)
	} else if !errors.Is(err, os.ErrNotExist) {
		return refuse(api.LinkOutcomeUnavailable)
	}
	if err := linkFile(filepath.Join(p.root, name), destination); err != nil {
		return refuse(linkErrorOutcome(err))
	}
	return api.ResultLink{Outcome: api.LinkOutcomeLinked, Receipt: accepted.Receipt, Size: info.Size()}, nil
}

// linkErrorOutcome names why a hard link could not be made.
func linkErrorOutcome(err error) api.LinkOutcome {
	switch {
	case errors.Is(err, os.ErrExist):
		return api.LinkOutcomeExists
	case otherVolume(err):
		return api.LinkOutcomeOtherVolume
	case noHardLinks(err):
		return api.LinkOutcomeNoHardLinks
	case errors.Is(err, os.ErrPermission):
		return api.LinkOutcomeForbidden
	}
	return api.LinkOutcomeUnavailable
}
