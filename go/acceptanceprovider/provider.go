// Package acceptanceprovider supplies service-owned recoverable admission to the
// existing FileStore. It does not execute jobs or provide downstream deduplication.
package acceptanceprovider

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	cas "github.com/openabstractions/abstraction-cas/go"
	job "github.com/openabstractions/abstraction-job/go"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

const (
	// These guarantees concern admission to the job store, not execution effects.
	GuaranteeCallerExit           = "abstraction.job/caller-exit@1"
	GuaranteeServiceRestart       = "abstraction.job/service-restart@1"
	GuaranteeReconciliation       = "abstraction.job/reconciliation@1"
	MinimumRetentionMs      int64 = 24 * 60 * 60 * 1000
	MaxSpecBytes                  = 1024 * 1024
	MaxCallerScopeBytes           = 4096
)

func Guarantees() []string {
	return []string{GuaranteeCallerExit, GuaranteeServiceRestart, GuaranteeReconciliation}
}

type configuration struct {
	Version      int
	Owner, Epoch string
	RetentionMs  int64
}

// journal is private provider recovery metadata, never an application contract.
// prepared is durable acceptance before the single, stable-ID FileStore insert.
// published means insertion succeeded; a missing published record is corruption,
// not permission to create it again. sealed prevents delayed acceptance forever.
type journal struct {
	Version   int
	Scope     string
	Identity  api.RequestIdentity
	Phase     string
	Arguments *api.Submission
	Receipt   *api.Receipt
	Reason    string
}

type Provider struct {
	root   string
	config configuration
	store  *job.FileStore
	// Only tests inject crash boundaries; production has no callbacks or effects.
	fault func(string) error
}

// Open is a service configuration API. root is private, local, service-owned
// storage on a filesystem supported by CAS. Never expose it through a client API.
// Recovery completes prepared insertions before the provider becomes available.
// Journal/epoch records are retained indefinitely; no expiry or rotation occurs.
func Open(root, logicalOwner string) (*Provider, error) {
	if root == "" || logicalOwner == "" || len(logicalOwner) > 1024 || !utf8.ValidString(logicalOwner) {
		return nil, fmt.Errorf("acceptance: root and logical owner required")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	p := &Provider{root: root}
	if err = os.MkdirAll(p.requests(), 0700); err != nil {
		return nil, err
	}
	if err = regularFile(filepath.Join(root, "acceptance", "owner.json")); err != nil {
		return nil, err
	}
	err = cas.Change(filepath.Join(root, "acceptance", "owner.json"), func(cur []byte) ([]byte, error) {
		if cur == nil {
			p.config = configuration{Version: 1, Owner: logicalOwner, Epoch: job.NewID(), RetentionMs: MinimumRetentionMs}
			return json.Marshal(p.config)
		}
		if err := strict(cur, &p.config); err != nil {
			return nil, err
		}
		if p.config.Version != 1 || p.config.Owner != logicalOwner || p.config.Epoch == "" || p.config.RetentionMs != MinimumRetentionMs {
			return nil, fmt.Errorf("acceptance: incompatible owner configuration")
		}
		return cur, nil
	})
	if err != nil {
		return nil, err
	}
	p.store, err = job.NewFileStore(root)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(p.requests())
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(p.requests(), entry.Name())
		if err := regularFile(path); err != nil {
			return nil, err
		}
		b, err := cas.Read(path)
		if err != nil {
			return nil, err
		}
		j, err := p.decodeJournal(b)
		if err != nil {
			return nil, err
		}
		if p.requestPath(j.Scope, j.Identity) != path {
			return nil, fmt.Errorf("acceptance: journal identity mismatch")
		}
		if j.Phase != "sealed" {
			if err := p.materialize(path); err != nil {
				return nil, err
			}
		}
	}
	return p, nil
}

// Bind takes the authenticated, authorized caller scope from the receiving
// boundary. It must not receive an unverified request field or upstream claim.
// An empty scope always yields forbidden (including on history-window lookup).
func (p *Provider) Bind(authenticatedCallerScope string) api.RecoverableAcceptance {
	if len(authenticatedCallerScope) > MaxCallerScopeBytes || !utf8.ValidString(authenticatedCallerScope) {
		authenticatedCallerScope = ""
	}
	return &bound{provider: p, scope: authenticatedCallerScope}
}

type bound struct {
	provider *Provider
	scope    string
}

func (p *Provider) requests() string { return filepath.Join(p.root, "acceptance", "requests") }
func (p *Provider) requestPath(scope string, id api.RequestIdentity) string {
	b, _ := json.Marshal([]string{scope, "abstraction.job/acceptance@1", id.HistoryEpoch, id.Key})
	h := sha256.Sum256(b)
	return filepath.Join(p.requests(), hex.EncodeToString(h[:])+".json")
}
func strict(b []byte, v any) error {
	if len(b) == 0 || len(b) > 2*MaxSpecBytes {
		return fmt.Errorf("acceptance: invalid recovery record size")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("acceptance: trailing recovery data")
	}
	return nil
}

func regularFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("acceptance: recovery path is not a regular file")
	}
	return nil
}
func (p *Provider) decodeJournal(b []byte) (*journal, error) {
	var j journal
	if err := strict(b, &j); err != nil {
		return nil, err
	}
	if j.Version != 1 || j.Scope == "" || len(j.Scope) > MaxCallerScopeBytes || j.Identity.Key == "" || len(j.Identity.Key) > 1024 || j.Identity.HistoryEpoch != p.config.Epoch || len(j.Reason) > 1024 {
		return nil, fmt.Errorf("acceptance: invalid recovery identity")
	}
	if j.Phase == "sealed" {
		if j.Arguments != nil || j.Receipt != nil {
			return nil, fmt.Errorf("acceptance: contradictory seal")
		}
		return &j, nil
	}
	if (j.Phase != "prepared" && j.Phase != "published") || j.Arguments == nil || j.Receipt == nil {
		return nil, fmt.Errorf("acceptance: invalid recovery phase")
	}
	if len(j.Receipt.OperationId) > 128 {
		return nil, fmt.Errorf("acceptance: invalid operation identifier")
	}
	for _, c := range j.Receipt.OperationId {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c == '-') {
			return nil, fmt.Errorf("acceptance: invalid operation identifier")
		}
	}
	v := api.ReconcileEvidence(j.Scope, p.config.Owner, j.Identity, nil, api.Evidence{CallerScope: j.Scope, Identity: j.Identity, Arguments: j.Arguments, Receipt: j.Receipt, HistoryAvailable: true})
	if v.Outcome != "accepted" || j.Receipt.HistoryRetentionMs != p.config.RetentionMs {
		return nil, fmt.Errorf("acceptance: contradictory recovery receipt")
	}
	if len(j.Receipt.AcceptedGuarantees) != len(Guarantees()) {
		return nil, fmt.Errorf("acceptance: unsupported recovery guarantee")
	}
	for _, g := range j.Receipt.AcceptedGuarantees {
		if !slices.Contains(Guarantees(), g) {
			return nil, fmt.Errorf("acceptance: unsupported recovery guarantee")
		}
	}
	if err := validateSpec(*j.Arguments); err != nil {
		return nil, err
	}
	return &j, nil
}

func validateSpec(s api.Submission) error {
	if err := api.ValidateSubmission(s); err != nil {
		return err
	}
	if len(s.Identity.Key) > 1024 || len(s.Identity.HistoryEpoch) > 1024 || len(s.Kind) > 256 || len(s.RequiredGuarantees) > 64 || len(s.Spec) > MaxSpecBytes {
		return fmt.Errorf("acceptance: submission limit exceeded")
	}
	for _, g := range s.RequiredGuarantees {
		if len(g) > 256 {
			return fmt.Errorf("acceptance: guarantee limit exceeded")
		}
	}
	r := job.Record{ID: "validation", Kind: s.Kind, Spec: s.Spec, State: job.StatePending, CreatedAt: job.At(time.Now()), UpdatedAt: job.At(time.Now())}
	r.Progress.UpdatedAt = r.CreatedAt
	_, err := r.Encode()
	return err
}

func (b *bound) GetHistoryWindow() (api.HistoryWindow, error) {
	if b.scope == "" {
		return api.HistoryWindow{}, &api.ServiceError{Code: "forbidden", Message: "authenticated caller required"}
	}
	c := b.provider.config
	return api.HistoryWindow{LogicalOwner: c.Owner, HistoryEpoch: c.Epoch, MinimumRetentionMs: c.RetentionMs}, nil
}
func outcome(word, reason string) api.AcceptanceResult {
	return api.AcceptanceResult{Outcome: word, Reason: reason}
}

func (b *bound) Submit(s api.Submission) (api.AcceptanceResult, error) {
	if b.scope == "" {
		return outcome("forbidden", "authenticated caller required"), nil
	}
	if err := validateSpec(s); err != nil {
		return outcome("invalid", "invalid or oversized job submission"), nil
	}
	// Snapshot caller memory before retaining arguments in a transaction.
	s.Spec = slices.Clone(s.Spec)
	s.RequiredGuarantees = slices.Clone(s.RequiredGuarantees)
	return b.resolve(s.Identity, &s)
}
func (b *bound) Reconcile(id api.RequestIdentity) (api.AcceptanceResult, error) {
	if b.scope == "" {
		return outcome("forbidden", "authenticated caller required"), nil
	}
	return b.resolve(id, nil)
}

func (b *bound) resolve(id api.RequestIdentity, s *api.Submission) (api.AcceptanceResult, error) {
	p := b.provider
	if id.Key == "" || len(id.Key) > 1024 || id.HistoryEpoch == "" {
		return outcome("invalid", "request identity required"), nil
	}
	// Unknown/old epochs are never fresh namespaces eligible for acceptance.
	if id.HistoryEpoch != p.config.Epoch {
		return outcome("unknown", "history epoch unavailable"), nil
	}
	path := p.requestPath(b.scope, id)
	if err := regularFile(path); err != nil {
		return outcome("unknown", "owner recovery evidence unavailable"), nil
	}
	var j *journal
	err := cas.Change(path, func(cur []byte) ([]byte, error) {
		if cur != nil {
			var err error
			j, err = p.decodeJournal(cur)
			return cur, err
		}
		j = &journal{Version: 1, Scope: b.scope, Identity: id, Phase: "sealed", Reason: "identity sealed before acceptance"}
		if s != nil {
			for _, g := range s.RequiredGuarantees {
				if !slices.Contains(Guarantees(), g) {
					j.Reason = "unsupported guarantee: " + g
					return json.Marshal(j)
				}
			}
			j.Phase = "prepared"
			j.Arguments = s
			j.Receipt = &api.Receipt{Identity: id, LogicalOwner: p.config.Owner, OperationId: job.NewID(), AcceptedGuarantees: Guarantees(), HistoryRetentionMs: p.config.RetentionMs}
		}
		return json.Marshal(j)
	})
	if err != nil {
		return outcome("unknown", "owner recovery evidence unavailable"), nil
	}
	if j.Scope != b.scope || j.Identity != id {
		return outcome("unknown", "owner evidence mismatch"), nil
	}
	v := api.ReconcileEvidence(b.scope, p.config.Owner, id, s, api.Evidence{CallerScope: j.Scope, Identity: j.Identity, Arguments: j.Arguments, Receipt: j.Receipt, HistoryAvailable: true, SealedNonAcceptance: j.Phase == "sealed"})
	if j.Phase == "sealed" {
		v.Reason = j.Reason
	}
	if v.Outcome != "accepted" {
		return v, nil
	}
	if err := p.crashPoint("after-journal"); err != nil {
		return outcome("unknown", "acceptance reply unavailable"), nil
	}
	if err := p.materialize(path); err != nil {
		return outcome("unknown", "accepted operation recovery unavailable"), nil
	}
	if err := p.crashPoint("before-reply"); err != nil {
		return outcome("unknown", "acceptance reply unavailable"), nil
	}
	return v, nil
}

func (p *Provider) crashPoint(point string) error {
	if p.fault != nil {
		return p.fault(point)
	}
	return nil
}
func (p *Provider) materialize(path string) error {
	if err := regularFile(path); err != nil {
		return err
	}
	// The same CAS journal lock spans lookup, insertion and published transition.
	// A reconciling process cannot seal a prepared admission or allocate another ID.
	return cas.Change(path, func(cur []byte) ([]byte, error) {
		j, err := p.decodeJournal(cur)
		if err != nil {
			return nil, err
		}
		if j.Phase == "sealed" {
			return cur, nil
		}
		r, err := p.store.Load(j.Receipt.OperationId)
		if errors.Is(err, job.ErrNotFound) && j.Phase == "prepared" {
			_, err = p.store.Submit(job.Record{ID: j.Receipt.OperationId, Kind: j.Arguments.Kind, Spec: j.Arguments.Spec})
			if err != nil {
				return nil, err
			}
			if err = p.crashPoint("after-job"); err != nil {
				return nil, err
			}
			r, err = p.store.Load(j.Receipt.OperationId)
		}
		if err != nil {
			return nil, err
		}
		var a, c bytes.Buffer
		if json.Compact(&a, r.Spec) != nil || json.Compact(&c, j.Arguments.Spec) != nil || r.ID != j.Receipt.OperationId || r.Kind != j.Arguments.Kind || !bytes.Equal(a.Bytes(), c.Bytes()) {
			return nil, fmt.Errorf("acceptance: operation does not match admission")
		}
		if j.Phase == "published" {
			return cur, nil
		}
		j.Phase = "published"
		return json.Marshal(j)
	})
}

func (b *bound) CancelWork(id api.RequestIdentity) (api.CancellationResult, error) {
	if b.scope == "" {
		return api.CancellationResult{Outcome: "forbidden"}, nil
	}
	v, err := b.Reconcile(id)
	if err != nil || v.Outcome != "accepted" {
		return api.CancellationResult{Outcome: "unknown"}, nil
	}
	_, err = b.provider.store.SetIntent(v.Receipt.OperationId, job.WantCancel, b.scope)
	if errors.Is(err, job.ErrTerminal) {
		return api.CancellationResult{Outcome: "already_terminal"}, nil
	}
	if err != nil {
		return api.CancellationResult{Outcome: "unknown"}, nil
	}
	return api.CancellationResult{Outcome: "requested"}, nil
}
