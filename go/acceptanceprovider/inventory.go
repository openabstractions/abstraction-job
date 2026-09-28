package acceptanceprovider

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	cas "github.com/openabstractions/abstraction-cas/go"
	job "github.com/openabstractions/abstraction-job/go"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

const inventorySessions = 32
const inventoryIdle = 30 * time.Second
const inventoryScanEntries = 256
const inventoryReadBytes = 16 << 20
const inventoryPageBytes = 512 << 10

type inventoryState struct {
	mu       sync.Mutex
	closed   bool
	sessions map[string]*inventorySession
}
type inventorySession struct {
	scope, token string
	// account marks an account-wide operator listing (JOB-A13).
	account   bool
	dir       *os.File
	sequence  uint64
	lastInput string
	lastLimit int64
	replay    []byte
	pending   *api.OperationSnapshot
	expires   time.Time
	timer     *time.Timer
}

// CloseInventory closes enumeration handles and caches. It is idempotent and
// permanently refuses new enumeration. Durable execution has a separate lifetime;
// call this after request workers drain when shutting down the provider host.
func (p *Provider) CloseInventory() error {
	p.inventory.mu.Lock()
	defer p.inventory.mu.Unlock()
	p.inventory.closed = true
	var errs []error
	for key, s := range p.inventory.sessions {
		s.timer.Stop()
		if s.dir != nil {
			errs = append(errs, s.dir.Close())
		}
		s.replay = nil
		s.pending = nil
		delete(p.inventory.sessions, key)
	}
	return errors.Join(errs...)
}
func (p *Provider) expireInventory(key string, s *inventorySession) {
	p.inventory.mu.Lock()
	defer p.inventory.mu.Unlock()
	if p.inventory.sessions[key] != s {
		return
	}
	if left := time.Until(s.expires); left > 0 {
		s.timer.Reset(left)
		return
	}
	if s.dir != nil {
		_ = s.dir.Close()
	}
	s.replay = nil
	s.pending = nil
	delete(p.inventory.sessions, key)
}
func (p *Provider) BindInventory(scope string) api.JobInventory {
	if len(scope) > MaxCallerScopeBytes || !utf8.ValidString(scope) {
		scope = ""
	}
	return &bound{provider: p, scope: scope}
}
func inventoryRefusal(outcome api.InventoryOutcome) api.InventoryPage {
	return api.InventoryPage{Outcome: outcome, Snapshots: []api.OperationSnapshot{}}
}
func (b *bound) ListWork(cursor string, limit int64) (api.InventoryPage, error) {
	return b.listWork(cursor, limit, false)
}

// listWork enumerates the caller's own scope, or every scope when account is
// true. A session and its cursors belong to one caller and one of the two
// listings.
func (b *bound) listWork(cursor string, limit int64, account bool) (api.InventoryPage, error) {
	if b.scope == "" {
		return inventoryRefusal(api.InventoryOutcomeForbidden), nil
	}
	if limit < 1 || limit > 64 || len(cursor) > 128 {
		return inventoryRefusal(api.InventoryOutcomeInvalid), nil
	}
	p := b.provider
	p.inventory.mu.Lock()
	defer p.inventory.mu.Unlock()
	if p.inventory.closed {
		return inventoryRefusal(api.InventoryOutcomeUnavailable), nil
	}
	var s *inventorySession
	if cursor == "" {
		if len(p.inventory.sessions) >= inventorySessions {
			return inventoryRefusal(api.InventoryOutcomeUnavailable), nil
		}
		var key [16]byte
		if _, err := rand.Read(key[:]); err != nil {
			return inventoryRefusal(api.InventoryOutcomeUnavailable), nil
		}
		dir, err := os.Open(p.requests())
		if err != nil {
			return inventoryRefusal(api.InventoryOutcomeUnavailable), nil
		}
		s = &inventorySession{scope: b.scope, account: account, token: hex.EncodeToString(key[:]), dir: dir, expires: time.Now().Add(inventoryIdle)}
		if p.inventory.sessions == nil {
			p.inventory.sessions = map[string]*inventorySession{}
		}
		p.inventory.sessions[s.token] = s
		s.timer = time.AfterFunc(inventoryIdle, func() { p.expireInventory(s.token, s) })
	} else {
		token, word, ok := strings.Cut(cursor, ":")
		n, err := strconv.ParseUint(word, 10, 64)
		s = p.inventory.sessions[token]
		if !ok || err != nil || strconv.FormatUint(n, 10) != word || s == nil || s.scope != b.scope || s.account != account {
			return inventoryRefusal(api.InventoryOutcomeGap), nil
		}
		if time.Now().After(s.expires) {
			if s.dir != nil {
				_ = s.dir.Close()
			}
			s.timer.Stop()
			delete(p.inventory.sessions, token)
			return inventoryRefusal(api.InventoryOutcomeGap), nil
		}
		if cursor == s.lastInput && len(s.replay) > 0 {
			if limit != s.lastLimit {
				return inventoryRefusal(api.InventoryOutcomeInvalid), nil
			}
			s.expires = time.Now().Add(inventoryIdle)
			var page api.InventoryPage
			_ = json.Unmarshal(s.replay, &page)
			return page, nil
		}
		if n != s.sequence || s.dir == nil {
			return inventoryRefusal(api.InventoryOutcomeGap), nil
		}
	}
	fail := func() (api.InventoryPage, error) {
		if s.dir != nil {
			_ = s.dir.Close()
		}
		s.timer.Stop()
		delete(p.inventory.sessions, s.token)
		return inventoryRefusal(api.InventoryOutcomeUnavailable), nil
	}
	page := api.InventoryPage{Outcome: api.InventoryOutcomePage, Snapshots: []api.OperationSnapshot{}}
	used := 0
	remaining := int64(inventoryReadBytes)
	for scanned := 0; scanned < inventoryScanEntries && len(page.Snapshots) < int(limit); {
		snapshot := s.pending
		s.pending = nil
		if snapshot == nil {
			// Two maximum bounded files must fit before consuming the next entry.
			if remaining < 2*(4*MaxSpecBytes+16385) {
				break
			}
			entries, err := s.dir.ReadDir(1)
			if err == io.EOF {
				page.Complete = true
				break
			}
			if err != nil {
				return fail()
			}
			scanned++
			entry := entries[0]
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			path := filepath.Join(p.requests(), entry.Name())
			data, err := inventoryRead(path, &remaining)
			if err != nil {
				return fail()
			}
			j, err := p.decodeJournal(data)
			if err != nil {
				return fail()
			}
			if filepath.Base(p.requestPath(j.Scope, j.Identity)) != entry.Name() {
				return fail()
			}
			if (!s.account && j.Scope != b.scope) || j.Phase == "sealed" {
				continue
			}
			// Inventory observes published records only; incomplete recovery is explicit.
			data, err = inventoryRead(filepath.Join(p.store.Root(), "jobs", j.Receipt.OperationID+".json"), &remaining)
			if err != nil {
				return fail()
			}
			record, err := job.Decode(data)
			if err != nil {
				return fail()
			}
			if record.ID != j.Receipt.OperationID {
				return fail()
			}
			snapshot = operationSnapshot(j, record, p.executor, j.ResultLost)
			if snapshot.Failure != nil && len(snapshot.Failure.Message) > 4096 {
				failure := *snapshot.Failure
				failure.Message = "operation failure diagnostic exceeds inventory limit"
				snapshot.Failure = &failure
			}
		}
		encoded, err := json.Marshal(snapshot)
		if err != nil || len(encoded) > inventoryPageBytes {
			return fail()
		}
		if used+len(encoded) > inventoryPageBytes {
			s.pending = snapshot
			break
		}
		used += len(encoded)
		page.Snapshots = append(page.Snapshots, *snapshot)
	}
	s.sequence++
	if page.Complete {
		_ = s.dir.Close()
		s.dir = nil
	} else {
		page.Next = s.token + ":" + strconv.FormatUint(s.sequence, 10)
	}
	// An initial page has no replayable input token: retrying the empty cursor
	// starts a new enumeration. Release its completed session immediately.
	// Completed continuation pages still retain their lost-reply replay cache.
	if page.Complete && cursor == "" {
		s.timer.Stop()
		delete(p.inventory.sessions, s.token)
		return page, nil
	}
	s.lastInput = cursor
	s.lastLimit = limit
	s.expires = time.Now().Add(inventoryIdle)
	s.replay, _ = json.Marshal(page)
	// Return an independent value; direct callers cannot modify the replay cache.
	return page, nil
}
func inventoryRead(path string, remaining *int64) ([]byte, error) {
	if err := regularFile(path); err != nil {
		return nil, err
	}
	// Inventory records are replaced through CAS. On Windows a reader can briefly
	// receive sharing violation or access denied while that replacement settles.
	// Use the same bounded, retrying read as other observers of those records.
	data, err := cas.ReadLimit(path, min(*remaining, 4*MaxSpecBytes+16384))
	*remaining -= int64(len(data))
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, os.ErrNotExist
	}
	return data, nil
}
