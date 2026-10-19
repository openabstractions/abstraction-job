package acceptanceprovider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	cas "github.com/openabstractions/abstraction-cas/go"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

const StorageFeatureCanonicalSubjects = "abstraction.job/journal-native-ownership@1"
const nativeOwnershipProfile = "owner-native-program@1"

// EnableCanonicalSubjects selects native account/Program ownership for this
// provider before it is shared. Hold AcquireHost throughout configuration and
// serving. Older direct users must be stopped and drained before activation.
func (p *Provider) EnableCanonicalSubjects() error {
	for range 16 {
		// Reuse recovery's validated journal associations. Capture/decode before
		// locking, then fence late ordinary reservations by exact roster+bytes.
		snapshot, err := p.activationSnapshot()
		if err != nil {
			return err
		}
		unlock, err := p.lockAdmission()
		if err != nil {
			return err
		}
		if err = snapshot.unchanged(p); err == nil {
			err = p.ensureFeature(StorageFeatureCanonicalSubjects)
		}
		if err == nil {
			for path, j := range snapshot.journals {
				if err = p.indexJournal(path, j); err != nil {
					break
				}
			}
		}
		if err == nil {
			p.canonicalSubjects = true
		}
		unlock()
		if errors.Is(err, errOwnershipChanged) {
			continue
		}
		return err
	}
	return errOwnershipChanged
}

func (p *Provider) lockAdmission() (func(), error) {
	return cas.LockContext(context.Background(), filepath.Join(p.root, "acceptance", "admission"), false)
}

func canonicalSubjectScope(b Binding) (string, error) {
	if b.valid() != nil || b.Subject == nil || b.Origin != "local" {
		return "", errors.New("acceptance: native ownership evidence required")
	}
	raw, err := json.Marshal([]string{nativeOwnershipProfile, b.Subject.AccountKind, b.Subject.Account, b.Subject.Program})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return nativeOwnershipProfile + ":" + hex.EncodeToString(digest[:]), nil
}

func retainedNativeScope(subject *AuthenticatedSubject, origin, evidence string) (string, error) {
	if subject == nil {
		return "", errors.New("acceptance: native subject unavailable")
	}
	legacy, err := LocalSubjectScope(subject.AccountKind, subject.Account, subject.Executable)
	if err != nil {
		return "", err
	}
	return canonicalSubjectScope(Binding{Scope: legacy, Subject: subject, Origin: origin, Evidence: evidence})
}

// OperationOwnedByBinding checks this exact operation using receiver-captured
// native ownership and the retained journal. Unavailable evidence is an error;
// another owner and an unknown operation are ordinary false results.
func (p *Provider) OperationOwnedByBinding(operationID string, binding Binding) (bool, error) {
	if err := binding.valid(); err != nil {
		return false, err
	}
	if !validOperationID(operationID) {
		return false, nil
	}
	p.subjectsMu.RLock()
	record, found := p.subjects[operationID]
	p.subjectsMu.RUnlock()
	if !found {
		return false, nil
	}
	j, err := p.indexedJournal(operationID, record)
	if err != nil {
		return false, err
	}
	b := bound{provider: p, scope: binding.Scope, binding: binding}
	return b.ownsJournal(j), nil
}

func journalBinding(j *journal) Binding {
	scope := j.Scope
	if j.OwnershipProfile == nativeOwnershipProfile && j.Subject != nil {
		scope, _ = LocalSubjectScope(j.Subject.AccountKind, j.Subject.Account, j.Subject.Executable)
	}
	return Binding{Scope: scope, Subject: j.Subject, Origin: j.SubjectOrigin, Evidence: j.SubjectEvidence}
}

func (p *Provider) checkCanonicalAdmission() error {
	c, err := checkConfiguration(p.root, p.config.Owner, p.config.Version, p.config.Execution, true)
	if err != nil {
		return err
	}
	if c == nil || c.Epoch != p.config.Epoch || c.Owner != p.config.Owner {
		return ErrIncompatibleStorage
	}
	if slices.Contains(c.Features, StorageFeatureCanonicalSubjects) != p.canonicalSubjects {
		return errors.New("acceptance: canonical native ownership must be configured")
	}
	for _, feature := range c.Features {
		p.features.Store(feature, true)
	}
	return nil
}

// Journal decoding (including deterministic executor validation) happens before
// the root lock. Reservation checks the exact captured bytes under that lock.
// A concurrent journal change refuses this call; it never becomes empty lookup.
type ownershipSnapshot struct {
	scope    string
	raw      map[string][]byte
	journals map[string]*journal
	all      bool
	names    []string // non-nil only for feature activation's complete roster
}

func (p *Provider) activationSnapshot() (*ownershipSnapshot, error) {
	ownerPath := filepath.Join(p.root, "acceptance", "owner.json")
	if err := regularFile(ownerPath); err != nil {
		return nil, err
	}
	ownerRaw, err := cas.ReadLimit(ownerPath, 4*MaxSpecBytes+16384)
	if err != nil {
		return nil, err
	}
	c, err := validateConfiguration(ownerRaw, p.config.Owner, p.config.Version, p.config.Execution, true)
	if err != nil {
		return nil, err
	}
	if c.Owner != p.config.Owner || c.Epoch != p.config.Epoch {
		return nil, ErrIncompatibleStorage
	}
	if !slices.Contains(c.Features, StorageFeatureCanonicalSubjects) {
		p.features.Delete(StorageFeatureCanonicalSubjects)
	}
	for _, feature := range c.Features {
		p.features.Store(feature, true)
	}
	s := &ownershipSnapshot{raw: map[string][]byte{}, journals: map[string]*journal{}, names: []string{}}
	s.raw[ownerPath] = ownerRaw
	entries, err := os.ReadDir(p.requests())
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		s.names = append(s.names, entry.Name())
		path := filepath.Join(p.requests(), entry.Name())
		if err = regularFile(path); err != nil {
			return nil, err
		}
		raw, err := cas.Read(path)
		if err != nil {
			return nil, err
		}
		j, err := p.decodeJournal(raw)
		if err != nil {
			return nil, err
		}
		if p.requestPath(j.Scope, j.Identity) != path {
			return nil, errors.New("acceptance: activation journal path mismatch")
		}
		s.raw[path], s.journals[path] = raw, j
	}
	return s, nil
}

var errOwnershipConflict = errors.New("acceptance: conflicting retained ownership occurrences")
var errOwnershipChanged = errors.New("acceptance: ownership evidence changed during reservation")

func (b *bound) ownershipSnapshot(id api.RequestIdentity) (*ownershipSnapshot, error) {
	p := b.provider
	if err := p.checkCanonicalAdmission(); err != nil {
		return nil, err
	}
	s := &ownershipSnapshot{scope: b.scope, raw: map[string][]byte{}, journals: map[string]*journal{}}
	canonical := ""
	legacyScope := b.scope
	if p.canonicalSubjects && b.binding.Subject != nil && b.binding.Origin == "local" {
		var err error
		canonical, err = canonicalSubjectScope(b.binding)
		if err != nil {
			return nil, err
		}
		s.scope = canonical
		s.all = true
		legacyScope = b.binding.Scope // exact current receiver's executable, also for prior attempts
	}
	paths := []string{p.requestPath(legacyScope, id)}
	if s.all {
		paths = append(paths, p.requestPath(canonical, id))
		// Open recovered these associations under the retained host lifecycle.
		// After activation every native reservation uses the canonical path;
		// direct/exact legacy access is the other path already listed above.
		p.subjectsMu.RLock()
		for _, record := range p.subjects {
			if record.subject == nil || record.origin != "local" {
				continue
			}
			legacy, err := LocalSubjectScope(record.subject.AccountKind, record.subject.Account, record.subject.Executable)
			if err != nil {
				p.subjectsMu.RUnlock()
				return nil, err
			}
			candidate, err := canonicalSubjectScope(Binding{Scope: legacy, Subject: record.subject, Origin: record.origin, Evidence: record.evidence})
			if err != nil {
				p.subjectsMu.RUnlock()
				return nil, err
			}
			if candidate == canonical {
				paths = append(paths, p.requestPath(record.scope, id))
			}
		}
		p.subjectsMu.RUnlock()
	}
	seen := map[string]bool{}
	known := map[string]bool{}
	p.subjectsMu.RLock()
	for _, record := range p.subjects {
		if slices.Contains(paths, record.journalPath) {
			known[record.journalPath] = true
		}
	}
	p.subjectsMu.RUnlock()
	matches := []*journal{}
	for _, path := range paths {
		if seen[path] {
			continue
		}
		seen[path] = true
		if err := regularFile(path); err != nil {
			return nil, err
		}
		raw, err := cas.Read(path)
		if err != nil {
			return nil, err
		}
		s.raw[path] = raw
		if raw == nil {
			if known[path] {
				return nil, errors.New("acceptance: retained journal unavailable")
			}
			continue
		}
		j, err := p.decodeJournal(raw)
		if err != nil {
			return nil, err
		}
		if p.requestPath(j.Scope, j.Identity) != path {
			return nil, errors.New("acceptance: journal path mismatch")
		}
		s.journals[path] = j
		if j.Identity != id {
			continue
		}
		match := j.Scope == legacyScope
		if s.all && j.Subject != nil && j.SubjectOrigin == "local" {
			candidate, err := canonicalSubjectScope(journalBinding(j))
			if err != nil {
				return nil, err
			}
			if j.Scope == legacyScope && candidate != canonical {
				return nil, errors.New("acceptance: exact legacy native provenance conflicts")
			}
			match = candidate == canonical
		}
		if match {
			matches = append(matches, j)
		}
	}
	if len(matches) > 1 {
		return nil, errOwnershipConflict
	}
	if len(matches) == 1 {
		s.scope = matches[0].Scope
	}
	return s, nil
}

func (s *ownershipSnapshot) unchanged(p *Provider) error {
	if s.names != nil {
		entries, err := os.ReadDir(p.requests())
		if err != nil {
			return err
		}
		names := []string{}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".json") {
				names = append(names, entry.Name())
			}
		}
		if !slices.Equal(names, s.names) {
			return errOwnershipChanged
		}
	}
	for path, expected := range s.raw {
		if err := regularFile(path); err != nil {
			return err
		}
		actual, err := cas.Read(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(actual, expected) {
			return errOwnershipChanged
		}
	}
	return nil
}

func (b *bound) ownsJournal(j *journal) bool {
	if !b.provider.canonicalSubjects || b.binding.Subject == nil || b.binding.Origin != "local" || j.Subject == nil || j.SubjectOrigin != "local" {
		return j.Scope == b.scope
	}
	current, err := canonicalSubjectScope(b.binding)
	if err != nil {
		return false
	}
	prior, err := retainedNativeScope(j.Subject, j.SubjectOrigin, j.SubjectEvidence)
	return err == nil && prior == current
}
