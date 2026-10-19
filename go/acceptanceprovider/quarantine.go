package acceptanceprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	cas "github.com/openabstractions/abstraction-cas/go"
)

// unreadableFolder holds journals the provider could not decode at start. It
// sits inside the requests folder and its name has no .json suffix, so the
// start scan never reads it. Nothing in the provider removes what it holds.
const unreadableFolder = "unreadable"

// errJournalProfile marks a journal this provider's execution profile cannot
// recover: a guarantee or execution specification another profile recorded.
// The journal is sound for that profile. The start refuses the store, as it
// did before, and moves nothing. Its text keeps
// the messages these refusals have always carried.
var errJournalProfile = errors.New("acceptance")

// journalUnknownSchema reports a journal a newer provider wrote: its Version
// is above this store's, or it carries a field this provider does not know at
// this store's version. Such a journal is sound and this reader is too old
// (JOB-D7), so the start scan leaves it in place.
func (p *Provider) journalUnknownSchema(b []byte) bool {
	var probe struct{ Version int }
	if json.Unmarshal(b, &probe) != nil {
		return false
	}
	if probe.Version > p.config.Version {
		return true
	}
	if probe.Version != p.config.Version || len(b) > 4*MaxSpecBytes+16384 {
		return false
	}
	// The strict decode refused it. A lenient decode of the on-disk document
	// that succeeds isolates the refusal to an unknown field.
	var doc journalDocument
	return json.Unmarshal(b, &doc) == nil && strict(b, new(journal)) != nil
}

// loadStartJournal decodes one journal for the start scan. A journal that
// cannot be decoded, or whose identity does not name its path, is moved into
// the unreadable folder and skipped. A newer-schema journal is skipped in
// place. Both write one log line and answer a nil journal and nil error, so
// the provider starts with the rest. A journal another execution profile
// recorded answers its error and the start refuses the store.
func (p *Provider) loadStartJournal(path string, b []byte) (*journal, error) {
	j, err := p.decodeJournal(b)
	if err == nil && p.requestPath(j.Scope, j.Identity) != path {
		err = errors.New("acceptance: journal identity mismatch")
	}
	if err == nil {
		return j, nil
	}
	if p.journalUnknownSchema(b) {
		log.Printf("acceptance: journal %s has a newer schema (%v); it stays in place for a newer provider and this start skips it", path, err)
		return nil, nil
	}
	if errors.Is(err, errJournalProfile) {
		return nil, err
	}
	moved, moveErr := p.quarantineJournal(path)
	if moveErr != nil {
		log.Printf("acceptance: journal %s is unreadable (%v) and could not be moved aside (%v); this start skips it", path, err, moveErr)
		return nil, nil
	}
	if moved == "" {
		// The journal is gone, or a concurrent writer made it readable. A
		// readable journal is indexed as any other.
		b, readErr := cas.Read(path)
		if readErr != nil || b == nil {
			return nil, nil
		}
		if j, err := p.decodeJournal(b); err == nil && p.requestPath(j.Scope, j.Identity) == path {
			return j, nil
		}
		return nil, nil
	}
	log.Printf("acceptance: journal %s is unreadable (%v); moved it to %s and started without it", path, err, moved)
	return nil, nil
}

// holdUnjournaledWork runs after a start skipped a journal. Every unfinished
// operation with no indexed journal is held: its subject is unknown, so a
// standing decision (JOB-A18) cannot be made for it and none of it runs. The
// record itself stays as it is. A store List that reads only part of the
// records holds what it read.
func (p *Provider) holdUnjournaledWork() {
	records, err := p.store.List()
	if err != nil {
		log.Printf("acceptance: listing work after skipping a journal read only part of the store: %v", err)
	}
	p.subjectsMu.Lock()
	defer p.subjectsMu.Unlock()
	for _, r := range records {
		if r == nil || r.State.Terminal() {
			continue
		}
		if _, ok := p.subjects[r.ID]; ok {
			continue
		}
		if p.journalLost == nil {
			p.journalLost = make(map[string]bool)
		}
		p.journalLost[r.ID] = true
		log.Printf("acceptance: operation %s has no readable journal; a standing rule cannot decide it again, so it holds the work", r.ID)
	}
}

// quarantinedJournals reports whether the unreadable folder holds any entry.
// A folder that cannot be read for any reason other than its absence counts
// as holding one, so work is never released on an unreadable answer.
func (p *Provider) quarantinedJournals() bool {
	folder := filepath.Join(p.requests(), unreadableFolder)
	if _, err := os.Lstat(folder); errors.Is(err, os.ErrNotExist) {
		return false
	}
	entries, err := os.ReadDir(folder)
	if err != nil {
		log.Printf("acceptance: unreadable journal folder could not be read (%v); unjournaled work stays held", err)
		return true
	}
	return len(entries) > 0
}

// quarantineJournal renames an unreadable journal into the unreadable folder
// under a timestamped name that no existing file holds. It holds the journal's
// CAS lock and reads the journal again first, so a journal a concurrent writer
// replaced with a readable version stays where it is. It answers "" when the
// journal is already gone or now reads.
func (p *Provider) quarantineJournal(path string) (string, error) {
	unlock, err := cas.LockContext(context.Background(), path, false)
	if err != nil {
		return "", err
	}
	defer unlock()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if j, err := p.decodeJournal(b); err == nil && p.requestPath(j.Scope, j.Identity) == path {
		return "", nil
	}
	folder := filepath.Join(p.requests(), unreadableFolder)
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return "", err
	}
	base := filepath.Base(path)
	stem := base[:len(base)-len(filepath.Ext(base))]
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	for n := 0; n < 1000; n++ {
		name := fmt.Sprintf("%s-%s.json", stem, stamp)
		if n > 0 {
			name = fmt.Sprintf("%s-%s-%d.json", stem, stamp, n)
		}
		target := filepath.Join(folder, name)
		if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := os.Rename(path, target); err != nil {
			return "", err
		}
		return target, nil
	}
	return "", errors.New("acceptance: no free name in the unreadable folder")
}
