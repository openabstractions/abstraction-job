package acceptanceprovider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/openabstractions/abstraction-identity/fileid"
	job "github.com/openabstractions/abstraction-job/go"
)

// TmpRemoval reports one removed service-owned name, relative to this provider.
// Other hard-link names and caller destinations are never collection targets.
type TmpRemoval struct {
	OperationID string
	Name        string
}

type tmpFile struct {
	name   string
	result bool
}

type tmpCollectionCandidate struct {
	journal *journal
	files   []tmpFile
}

// tmpCandidate reads retained admission and terminal state, never directory
// entries or lease age. Each call revalidates both owners under the removal lock.
// With partialsOnly, a completed operation's result is never a candidate.
func (p *Provider) tmpCandidate(id string, now time.Time, maxRetention time.Duration, partialsOnly bool) (tmpCollectionCandidate, error) {
	var candidate tmpCollectionCandidate
	p.subjectsMu.RLock()
	association, found := p.subjects[id]
	p.subjectsMu.RUnlock()
	if !found {
		return candidate, errors.New("acceptance: temporary allocation has no journal")
	}
	j, err := p.indexedJournal(id, association)
	if err != nil {
		return candidate, err
	}
	if j.Phase != "published" || j.Origin != "" || j.Receipt == nil || j.Receipt.OperationID != id {
		return candidate, nil
	}
	record, err := p.loadOperationRecord(id)
	if err != nil {
		return candidate, err
	}
	if err := validateOperationRecord(j, record); err != nil {
		return candidate, err
	}
	if !record.State.Terminal() {
		return candidate, nil
	}
	if record.UpdatedAt.IsZero() || record.UpdatedAt.After(now) {
		return candidate, errors.New("acceptance: temporary lifetime time is missing or future")
	}
	namer, ok := p.executor.(TmpNamer)
	if !ok {
		return candidate, nil
	}
	result, partial, err := namer.OperationTmpNames(record)
	if errors.Is(err, ErrResultNotNamed) {
		return candidate, nil
	}
	if err != nil {
		return candidate, err
	}
	candidate.journal = j
	if record.State == job.StateComplete && !j.ResultLost && !partialsOnly {
		retention := p.resultRetention()
		if retention <= 0 || retention > int64((1<<63-1)/time.Millisecond) {
			return candidate, errors.New("acceptance: result retention unavailable")
		}
		if !now.Before(record.UpdatedAt.Add(time.Duration(retention) * time.Millisecond).Add(maxRetention)) {
			candidate.files = append(candidate.files, tmpFile{name: result, result: true})
		}
	}
	if (record.State == job.StateFailed || record.State == job.StateCancelled) && !now.Before(record.UpdatedAt.Add(maxRetention)) {
		candidate.files = append(candidate.files, tmpFile{name: partial})
	}
	return candidate, nil
}

// CollectTmp performs one explicitly requested pass. It schedules no work.
// The host supplies a freshly resolved positive temporary grace on every call.
// Terminal journals stay retained, so interrupted unlink/loss recording repeats.
func (p *Provider) CollectTmp(ctx context.Context, now time.Time, maxRetention time.Duration) ([]TmpRemoval, error) {
	return p.collectTmp(ctx, now, maxRetention, false)
}

// CollectEndedPartials performs one explicitly requested pass over partial
// files only. It removes the service-allocated partial of a failed or
// cancelled operation once minAge has passed since that operation ended. A
// running, pending or transferred operation, a completed result, migrated
// work and caller destinations are never candidates. The host supplies a
// freshly resolved positive age on every call.
func (p *Provider) CollectEndedPartials(ctx context.Context, now time.Time, minAge time.Duration) ([]TmpRemoval, error) {
	return p.collectTmp(ctx, now, minAge, true)
}

func (p *Provider) collectTmp(ctx context.Context, now time.Time, maxRetention time.Duration, partialsOnly bool) ([]TmpRemoval, error) {
	if maxRetention <= 0 || now.IsZero() {
		return nil, errors.New("acceptance: positive temporary retention and clock required")
	}
	if _, ok := p.executor.(TmpNamer); !ok {
		return nil, nil
	}
	p.subjectsMu.RLock()
	ids := make([]string, 0, len(p.subjects))
	for id := range p.subjects {
		ids = append(ids, id)
	}
	p.subjectsMu.RUnlock()
	sort.Strings(ids)
	var removed []TmpRemoval
	var problems []error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return removed, errors.Join(append(problems, err)...)
		}
		// Candidate admission and effect share the write lock. A completed
		// ReadResult or LinkResult is re-observed before deciding collection.
		p.resultMu.Lock()
		candidate, err := p.tmpCandidate(id, now, maxRetention, partialsOnly)
		if err == nil {
			for _, file := range candidate.files {
				var changed bool
				changed, err = p.removeTmp(ctx, file.name)
				if err != nil {
					break
				}
				if changed {
					removed = append(removed, TmpRemoval{OperationID: id, Name: file.name})
				}
				if file.result {
					err = p.recordResultLost(candidate.journal.Scope, candidate.journal.Identity)
					if err != nil {
						break
					}
				}
			}
		}
		p.resultMu.Unlock()
		if err != nil {
			problems = append(problems, fmt.Errorf("acceptance: collect %s: %w", id, err))
		}
	}
	return removed, errors.Join(problems...)
}

// tmpLinkCount is the native file-identity observation, with an inert seam for
// the unknown-count negative fixture. Unknown and aliased files are kept.
var tmpLinkCount = func(id fileid.FileID) int { return id.Links() }

func (p *Provider) removeTmp(ctx context.Context, name string) (bool, error) {
	if !filepath.IsLocal(name) || strings.Contains(name, `\`) || strings.HasSuffix(name, ".part") {
		return false, errors.New("temporary name is not a local allocation")
	}
	parts := strings.Split(name, "/")
	if len(parts) != 2 || (parts[0] != "work" && parts[0] != "results") || parts[1] == "" {
		return false, errors.New("temporary name is outside the owned allocation")
	}
	area, err := os.OpenRoot(p.root)
	if err != nil {
		return false, err
	}
	defer area.Close()
	info, err := area.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return false, errors.New("temporary allocation is not a regular file")
	}
	h, err := fileid.Open(ctx, fileid.Self, filepath.Join(p.root, name), fileid.Options{Links: fileid.RefuseLinks})
	if err != nil {
		return false, err
	}
	held, statErr := h.File().Stat()
	links := tmpLinkCount(h.FileID())
	closeErr := h.Close()
	if statErr != nil || closeErr != nil || !os.SameFile(info, held) || links != 1 {
		return false, errors.New("temporary allocation is changed, aliased or has unknown link count")
	}
	if err := area.Remove(name); err != nil {
		return false, err
	}
	return true, nil
}
