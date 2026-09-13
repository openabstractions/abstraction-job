// Package acceptanceprovider supplies service-owned recoverable admission to the
// existing FileStore, with an optional service-owned executor.
package acceptanceprovider

import (
	"bytes"
	"context"
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
	Execution    string `json:",omitempty"`
}

// journal is private provider recovery metadata, never an application contract.
// prepared is durable acceptance before the single, stable-ID FileStore insert.
// published means insertion succeeded; a missing published record is corruption,
// not permission to create it again. sealed prevents delayed acceptance forever.
type journal struct {
	Version      int
	Scope        string
	Identity     api.RequestIdentity
	Phase        string
	Arguments    *api.Submission
	Receipt      *api.Receipt
	Reason       string
	WorkSpec     []byte   `json:",omitempty"`
	WorkRequires []string `json:",omitempty"`
}

func (j *journal) workSpec() []byte {
	if j.Version == 2 {
		return j.WorkSpec
	}
	return j.Arguments.Spec
}

type Provider struct {
	inventory inventoryState
	root      string
	config    configuration
	store     *job.FileStore
	executor  Executor
	// Tests inject failures at durable admission boundaries.
	fault func(string) error
}

// Executor is a service-owned worker. Prepare is a deterministic, side-effect-free
// transformation, stable across versions sharing a Profile. Recovery verifies it.
// Serve uses the provider's store and the host lifetime, never a request context.
type Executor interface {
	Profile() string
	Prepare(operationID, kind string, spec []byte) ([]byte, error)
	Serve(context.Context, job.Store) error
}

// GuaranteedExecutor prepares execution requirements under the same deterministic,
// side-effect-free contract as Prepare. A profile preserves the meaning of every
// advertised promise across restarts. Dependencies may remain unavailable while
// retained work waits; preparation must not reinterpret it using current offers.
type GuaranteedExecutor interface {
	Executor
	ExecutionGuarantees() []string
	// required contains execution promises after admission promises are removed.
	// Return the immutable work specification and its placement requirements.
	PrepareWithGuarantees(operationID, kind string, spec []byte, required []string) ([]byte, []string, error)
}

// SupportedGuarantees is the vocabulary this configured provider can negotiate.
func (p *Provider) SupportedGuarantees() []string {
	values := Guarantees()
	if e, ok := p.executor.(GuaranteedExecutor); ok {
		for _, g := range e.ExecutionGuarantees() {
			if g != "" && len(g) <= 256 && utf8.ValidString(g) && !slices.Contains(values, g) {
				values = append(values, g)
			}
		}
	}
	return values
}

func (p *Provider) acceptedGuarantees(required []string) ([]string, error) {
	values := Guarantees()
	for _, g := range required {
		if !slices.Contains(p.SupportedGuarantees(), g) {
			return nil, fmt.Errorf("unsupported guarantee: %s", g)
		}
		if !slices.Contains(values, g) {
			values = append(values, g)
		}
	}
	return values, nil
}

func (p *Provider) prepare(id string, s api.Submission) ([]byte, []string, error) {
	extra := []string{}
	for _, g := range s.RequiredGuarantees {
		if !slices.Contains(Guarantees(), g) {
			extra = append(extra, g)
		}
	}
	if len(extra) != 0 {
		e, ok := p.executor.(GuaranteedExecutor)
		if !ok {
			return nil, nil, errors.New("execution guarantees unavailable")
		}
		work, requires, err := e.PrepareWithGuarantees(id, s.Kind, bytes.Clone(s.Spec), slices.Clone(extra))
		if len(requires) > 64 {
			return nil, nil, errors.New("too many execution requirements")
		}
		for _, g := range requires {
			if g == "" || len(g) > 256 || !utf8.ValidString(g) {
				return nil, nil, errors.New("invalid execution requirement")
			}
		}
		return bytes.Clone(work), slices.Clone(requires), err
	}
	if p.executor == nil {
		return nil, nil, nil
	}
	work, err := p.executor.Prepare(id, s.Kind, bytes.Clone(s.Spec))
	return bytes.Clone(work), nil, err
}

func OpenWithExecutor(root, logicalOwner string, executor Executor) (*Provider, error) {
	return open(root, logicalOwner, executor, false)
}

// OpenManaged opens private service-owned storage and retains its logical owner
// across restarts. A fresh root allocates its owner under the existing owner.json
// CAS lock. Existing acceptance configuration must have a compatible profile;
// unowned legacy jobs are never adopted. This is a service configuration API.
func OpenManaged(root string, executor Executor) (*Provider, error) {
	return open(root, "", executor, true)
}

// LogicalOwner is the persistent receipt and discovery identity of this provider.
func (p *Provider) LogicalOwner() string { return p.config.Owner }

func (p *Provider) Execute(ctx context.Context) error {
	if p.executor == nil {
		return errors.New("acceptance: execution not configured")
	}
	return p.executor.Serve(ctx, p.store)
}

// Open is a service configuration API. root is private, local, service-owned
// storage on a filesystem supported by CAS. Never expose it through a client API.
// Recovery completes prepared insertions before the provider becomes available.
// Journal/epoch records are retained indefinitely; no expiry or rotation occurs.
func Open(root, logicalOwner string) (*Provider, error) {
	return open(root, logicalOwner, nil, false)
}

func open(root, logicalOwner string, executor Executor, managed bool) (*Provider, error) {
	if root == "" || (!managed && logicalOwner == "") || len(logicalOwner) > 1024 || !utf8.ValidString(logicalOwner) {
		return nil, fmt.Errorf("acceptance: root and logical owner required")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	version, profile, err := storageProfile(executor)
	if err != nil {
		return nil, err
	}
	// Refuse incompatible existing metadata before creating directories or CAS
	// lock files. The locked callback repeats validation against races.
	if _, err = checkConfiguration(root, logicalOwner, version, profile, managed); err != nil {
		return nil, err
	}
	p := &Provider{root: root, executor: executor}
	if err = os.MkdirAll(p.requests(), 0700); err != nil {
		return nil, err
	}
	if err = regularFile(filepath.Join(root, "acceptance", "owner.json")); err != nil {
		return nil, err
	}
	err = cas.Change(filepath.Join(root, "acceptance", "owner.json"), func(cur []byte) ([]byte, error) {
		if cur == nil {
			// Execution roots belong exclusively to this admission provider.
			// Legacy records carry no evidence authorizing their adoption here.
			if executor != nil || managed {
				if err := freshRoot(root); err != nil {
					return nil, err
				}
			}
			if managed {
				logicalOwner = "openabstractions.job/" + job.NewID()
			}
			p.config = configuration{Version: version, Owner: logicalOwner, Epoch: job.NewID(), RetentionMs: MinimumRetentionMs, Execution: profile}
			return json.Marshal(p.config)
		}
		var err error
		p.config, err = validateConfiguration(cur, logicalOwner, version, profile, managed)
		if err != nil {
			return nil, err
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
	if len(b) == 0 || len(b) > 4*MaxSpecBytes+16384 {
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
	if j.Version != p.config.Version || j.Scope == "" || len(j.Scope) > MaxCallerScopeBytes || j.Identity.Key == "" || len(j.Identity.Key) > 1024 || j.Identity.HistoryEpoch != p.config.Epoch || len(j.Reason) > 1024 {
		return nil, fmt.Errorf("acceptance: invalid recovery identity")
	}
	if j.Phase == "sealed" {
		if j.Arguments != nil || j.Receipt != nil || len(j.WorkSpec) != 0 || len(j.WorkRequires) != 0 {
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
	expectedGuarantees, err := p.acceptedGuarantees(j.Arguments.RequiredGuarantees)
	if err != nil || len(j.Receipt.AcceptedGuarantees) != len(expectedGuarantees) {
		return nil, fmt.Errorf("acceptance: unsupported recovery guarantee")
	}
	for _, g := range j.Receipt.AcceptedGuarantees {
		if !slices.Contains(expectedGuarantees, g) {
			return nil, fmt.Errorf("acceptance: unsupported recovery guarantee")
		}
	}
	if err := validateSpec(*j.Arguments); err != nil {
		return nil, err
	}
	if p.executor != nil {
		expected, requires, err := p.prepare(j.Receipt.OperationId, *j.Arguments)
		if err != nil || !bytes.Equal(expected, j.WorkSpec) || !slices.Equal(requires, j.WorkRequires) {
			return nil, errors.New("acceptance: incompatible execution specification")
		}
	} else if len(j.WorkSpec) != 0 || len(j.WorkRequires) != 0 {
		return nil, errors.New("acceptance: unexpected execution specification")
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
		return api.HistoryWindow{}, &api.ServiceError{Code: "forbidden", Message: "caller not authorized"}
	}
	c := b.provider.config
	return api.HistoryWindow{LogicalOwner: c.Owner, HistoryEpoch: c.Epoch, MinimumRetentionMs: c.RetentionMs}, nil
}
func outcome(word, reason string) api.AcceptanceResult {
	return api.AcceptanceResult{Outcome: word, Reason: reason}
}

func (b *bound) Submit(s api.Submission) (api.AcceptanceResult, error) {
	if b.scope == "" {
		return outcome("forbidden", "caller not authorized"), nil
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
		return outcome("forbidden", "caller not authorized"), nil
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
		j = &journal{Version: p.config.Version, Scope: b.scope, Identity: id, Phase: "sealed", Reason: "identity sealed before acceptance"}
		if s != nil {
			accepted, err := p.acceptedGuarantees(s.RequiredGuarantees)
			if err != nil {
				j.Reason = err.Error()
				return json.Marshal(j)
			}
			operationID := job.NewID()
			if p.executor != nil {
				work, requires, err := p.prepare(operationID, *s)
				if err != nil || len(work) > MaxSpecBytes || !json.Valid(work) {
					j.Reason = "configured executor refused this work"
					return json.Marshal(j)
				}
				j.WorkSpec = bytes.Clone(work)
				j.WorkRequires = slices.Clone(requires)
			}
			j.Phase = "prepared"
			j.Arguments = s
			j.Receipt = &api.Receipt{Identity: id, LogicalOwner: p.config.Owner, OperationId: operationID, AcceptedGuarantees: accepted, HistoryRetentionMs: p.config.RetentionMs}
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
	return cas.ChangeLimit(path, maxOperationRecordBytes, func(cur []byte) ([]byte, error) {
		j, err := p.decodeJournal(cur)
		if err != nil {
			return nil, err
		}
		if j.Phase == "sealed" {
			return cur, nil
		}
		r, err := p.loadOperationRecord(j.Receipt.OperationId)
		if errors.Is(err, job.ErrNotFound) && j.Phase == "prepared" {
			_, err = p.store.Submit(job.Record{ID: j.Receipt.OperationId, Kind: j.Arguments.Kind, Spec: j.workSpec(), Requires: slices.Clone(j.WorkRequires)})
			if err != nil {
				return nil, err
			}
			if err = p.crashPoint("after-job"); err != nil {
				return nil, err
			}
			r, err = p.loadOperationRecord(j.Receipt.OperationId)
		}
		if err != nil {
			return nil, err
		}
		var a, c bytes.Buffer
		if json.Compact(&a, r.Spec) != nil || json.Compact(&c, j.workSpec()) != nil || r.ID != j.Receipt.OperationId || r.Kind != j.Arguments.Kind || !bytes.Equal(a.Bytes(), c.Bytes()) || !slices.Equal(r.Requires, j.WorkRequires) {
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
