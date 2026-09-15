package acceptanceprovider

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	cas "github.com/openabstractions/abstraction-cas/go"
	job "github.com/openabstractions/abstraction-job/go"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

// LegacyReason is the machine-readable reason one legacy entry was not converted.
type LegacyReason string

const (
	// LegacyUnmapped: the operator mapping names no caller for this record.
	LegacyUnmapped LegacyReason = "unmapped"
	// LegacyActiveDelegation: non-terminal work handed to an external delegate.
	LegacyActiveDelegation LegacyReason = "active_delegation"
	// LegacyActiveLease: non-terminal work under a worker lease that has not expired.
	LegacyActiveLease LegacyReason = "active_lease"
	// LegacyNotTerminal: non-terminal work a legacy sweeper may still claim.
	LegacyNotTerminal LegacyReason = "not_terminal"
	// LegacyRecordChanged: record bytes differ from the digest the operator reviewed.
	LegacyRecordChanged LegacyReason = "record_changed"
	// LegacyIncompatibleWork: the mapped submission does not reproduce the
	// record's kind, work specification and requirements under this profile.
	LegacyIncompatibleWork LegacyReason = "incompatible_work"
	// LegacyUnreadable: the record cannot be read or decoded within service bounds.
	LegacyUnreadable LegacyReason = "unreadable"
	// LegacyUnrecognizedEntry: a jobs directory entry that is not a record or lock.
	LegacyUnrecognizedEntry LegacyReason = "unrecognized_entry"
	// LegacyMappedRecordMissing: the mapping names an operation with no record.
	LegacyMappedRecordMissing LegacyReason = "mapped_record_missing"
)

// ErrLegacyMigrationRefused reports that at least one entry was refused. Nothing
// was written. It matches ErrLegacyOwnership and ErrIncompatibleStorage.
var ErrLegacyMigrationRefused = fmt.Errorf("%w: legacy migration refused", ErrLegacyOwnership)

// ErrLegacyMigrationConflict reports existing ownership or a recorded migration
// that differs from the requested mapping. It matches ErrIncompatibleStorage.
var ErrLegacyMigrationConflict = fmt.Errorf("%w: legacy migration conflicts with recorded ownership", ErrIncompatibleStorage)

// ErrLegacyMappingInvalid reports a malformed operator mapping before any read.
var ErrLegacyMappingInvalid = errors.New("acceptance: invalid legacy ownership mapping")

// LegacyJob describes one entry of an unowned jobs directory for an operator
// authoring a mapping. Refusal is set when the entry itself cannot be converted.
type LegacyJob struct {
	OperationID  string
	State        job.State
	Kind         string
	Spec         json.RawMessage
	RecordSHA256 string
	Refusal      LegacyReason
}

// RecordedExecutionProfile reports the execution profile an owned root retains.
// owned is false when no owner configuration exists. Hosts use it to select the
// executor whose profile the root was created or migrated under.
func RecordedExecutionProfile(root string) (profile string, owned bool, err error) {
	if root == "" {
		return "", false, errors.New("acceptance: private root required")
	}
	data, err := readPrivate(filepath.Join(root, "acceptance", "owner.json"))
	if err != nil || data == nil {
		return "", false, err
	}
	var c configuration
	if err := strict(data, &c); err != nil {
		return "", false, fmt.Errorf("%w: unreadable owner configuration", ErrIncompatibleStorage)
	}
	return c.Execution, true, nil
}

// LegacyAssignment is the operator's authoritative statement that one exact
// record belongs to one authenticated caller scope and request key, and that the
// caller's submission was Kind/Spec/RequiredGuarantees. The history epoch is the
// migrated owner's epoch, reported in LegacyMigration.
type LegacyAssignment struct {
	OperationID        string
	RecordSHA256       string
	CallerScope        string
	Key                string
	Kind               string
	Spec               []byte
	RequiredGuarantees []string
}

type LegacyMapping struct {
	Assignments []LegacyAssignment
}

type LegacyRefusal struct {
	Entry  string
	Reason LegacyReason
}

type LegacyConversion struct {
	CallerScope string
	Receipt     api.Receipt
}

type LegacyMigration struct {
	LogicalOwner    string
	HistoryEpoch    string
	Converted       []LegacyConversion
	Refused         []LegacyRefusal
	AlreadyMigrated bool
}

// migrationRecord is private provenance written before any journal. Owner.json
// is written last; until then managed opens keep refusing the root.
type migrationRecord struct {
	Version     int
	Owner       string
	Epoch       string
	Storage     int
	Execution   string `json:",omitempty"`
	Assignments []LegacyAssignment
}

const legacyMigrationFile = "legacy-migration.json"

// legacyMigrationFault lets tests interrupt the commit at named boundaries.
var legacyMigrationFault func(string) error

func migrationPoint(name string) error {
	if legacyMigrationFault != nil {
		return legacyMigrationFault(name)
	}
	return nil
}

// InspectLegacyJobs lists an unowned jobs directory without writing. Operators
// use the digests to bind a mapping to the exact bytes they reviewed.
func InspectLegacyJobs(root string) ([]LegacyJob, error) {
	if root == "" {
		return nil, errors.New("acceptance: private root required")
	}
	entries, err := os.ReadDir(filepath.Join(root, "jobs"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := []LegacyJob{}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".json.lock") && entry.Type().IsRegular() {
			continue
		}
		id, ok := strings.CutSuffix(name, ".json")
		if !ok {
			out = append(out, LegacyJob{OperationID: name, Refusal: LegacyUnrecognizedEntry})
			continue
		}
		data, record, reason := readLegacyRecord(root, id)
		item := LegacyJob{OperationID: id, Refusal: reason}
		if data != nil {
			item.RecordSHA256 = digest(data)
		}
		if record != nil {
			item.State = record.State
			item.Kind = record.Kind
			item.Spec = slices.Clone(record.Spec)
			item.Refusal = activity(record, now)
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OperationID < out[j].OperationID })
	return out, nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func readLegacyRecord(root, id string) ([]byte, *job.Record, LegacyReason) {
	path := filepath.Join(root, "jobs", id+".json")
	if !validOperationID(id) || regularFile(path) != nil {
		return nil, nil, LegacyUnreadable
	}
	data, err := cas.ReadLimit(path, maxOperationRecordBytes)
	if err != nil || data == nil {
		return nil, nil, LegacyUnreadable
	}
	record, err := job.Decode(data)
	if err != nil || record.ID != id {
		return data, nil, LegacyUnreadable
	}
	return data, record, ""
}

// activity classifies whether a live predecessor or delegate may still act on
// the record. Only terminal records are convertible: active work is drained
// through the explicitly selected legacy provider first.
func activity(r *job.Record, now time.Time) LegacyReason {
	switch {
	case r.State.Terminal():
		return ""
	case r.Delegation != nil:
		return LegacyActiveDelegation
	case r.Lease.Held(now):
		return LegacyActiveLease
	default:
		return LegacyNotTerminal
	}
}

func validOperationID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c == '-') {
			return false
		}
	}
	return true
}

func normalizeMapping(m LegacyMapping) ([]LegacyAssignment, error) {
	if len(m.Assignments) == 0 {
		return nil, fmt.Errorf("%w: no assignments", ErrLegacyMappingInvalid)
	}
	out := make([]LegacyAssignment, 0, len(m.Assignments))
	operations := map[string]bool{}
	identities := map[[2]string]bool{}
	for _, a := range m.Assignments {
		if !validOperationID(a.OperationID) {
			return nil, fmt.Errorf("%w: invalid operation identifier %q", ErrLegacyMappingInvalid, a.OperationID)
		}
		if decoded, err := hex.DecodeString(a.RecordSHA256); err != nil || len(decoded) != sha256.Size || strings.ToLower(a.RecordSHA256) != a.RecordSHA256 {
			return nil, fmt.Errorf("%w: %s needs a lowercase SHA-256 record digest", ErrLegacyMappingInvalid, a.OperationID)
		}
		if a.CallerScope == "" || len(a.CallerScope) > MaxCallerScopeBytes || !utf8.ValidString(a.CallerScope) {
			return nil, fmt.Errorf("%w: %s needs an authenticated caller scope", ErrLegacyMappingInvalid, a.OperationID)
		}
		if a.Key == "" || len(a.Key) > 1024 || !utf8.ValidString(a.Key) {
			return nil, fmt.Errorf("%w: %s needs a request key", ErrLegacyMappingInvalid, a.OperationID)
		}
		if operations[a.OperationID] {
			return nil, fmt.Errorf("%w: %s assigned twice", ErrLegacyMappingInvalid, a.OperationID)
		}
		identity := [2]string{a.CallerScope, a.Key}
		if identities[identity] {
			return nil, fmt.Errorf("%w: one caller request identity assigned to two operations", ErrLegacyMappingInvalid)
		}
		operations[a.OperationID], identities[identity] = true, true
		s := api.Submission{Identity: api.RequestIdentity{Key: a.Key, HistoryEpoch: "mapping"}, Kind: a.Kind, Spec: a.Spec, RequiredGuarantees: a.RequiredGuarantees}
		if err := validateSpec(s); err != nil {
			return nil, fmt.Errorf("%w: %s submission: %v", ErrLegacyMappingInvalid, a.OperationID, err)
		}
		a.Spec = bytes.Clone(a.Spec)
		a.RequiredGuarantees = slices.Clone(a.RequiredGuarantees)
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OperationID < out[j].OperationID })
	return out, nil
}

// MigrateLegacy is an operator-invoked service configuration API. It converts an
// unowned legacy jobs directory into service-owned records using mapping as the
// sole ownership authority. Record files, work and result bytes are never
// rewritten; conversion adds published acceptance journals and then owner.json.
//
// Conversion is all or nothing. Every record must be terminal, mapped and match
// its reviewed digest, and every mapped submission must reproduce the record
// under executor's profile. Otherwise Refused names each entry, the error
// matches ErrLegacyMigrationRefused and nothing is written.
//
// Fencing: the commit holds AcquireHost, so a live service host or a concurrent
// migration refuses with ErrHostActive. Each record's digest and terminal state
// are re-verified under its own CAS lock while its journal is written, which
// excludes a concurrent legacy FileStore writer. A recorded migration pins the
// owner, epoch and mapping: repeating it is idempotent and any different mapping
// fails with ErrLegacyMigrationConflict, so no record gains a second owner.
func MigrateLegacy(root string, executor Executor, mapping LegacyMapping) (LegacyMigration, error) {
	if root == "" {
		return LegacyMigration{}, errors.New("acceptance: private root required")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return LegacyMigration{}, err
	}
	version, profile, err := storageProfile(executor)
	if err != nil {
		return LegacyMigration{}, err
	}
	assignments, err := normalizeMapping(mapping)
	if err != nil {
		return LegacyMigration{}, err
	}
	plan := migrationRecord{Version: 1, Storage: version, Execution: profile, Assignments: assignments}
	// A read-only pass first: refusals leave the root without even a host lock file.
	// Every other outcome, including verifying a completed migration, holds the
	// host lease and therefore refuses while a runtime host is running.
	result, recorded, err := evaluateMigration(root, executor, plan)
	if err != nil || len(result.Refused) != 0 {
		return result, err
	}
	lease, err := AcquireHost(root)
	if err != nil {
		return LegacyMigration{}, err
	}
	defer lease.Close()
	result, recorded, err = evaluateMigration(root, executor, plan)
	if err != nil || len(result.Refused) != 0 || result.AlreadyMigrated {
		return result, err
	}
	return commitMigration(root, executor, plan, recorded)
}

func sameMapping(a, b migrationRecord) bool {
	a.Owner, a.Epoch, b.Owner, b.Epoch = "", "", "", ""
	x, errX := json.Marshal(a)
	y, errY := json.Marshal(b)
	return errX == nil && errY == nil && bytes.Equal(x, y)
}

func (p *Provider) legacyJournal(a LegacyAssignment) ([]byte, *journal, error) {
	id := api.RequestIdentity{Key: a.Key, HistoryEpoch: p.config.Epoch}
	s := api.Submission{Identity: id, Kind: a.Kind, Spec: bytes.Clone(a.Spec), RequiredGuarantees: slices.Clone(a.RequiredGuarantees)}
	accepted, err := p.acceptedGuarantees(s.RequiredGuarantees)
	if err != nil {
		return nil, nil, err
	}
	j := &journal{Version: p.config.Version, Scope: a.CallerScope, Identity: id, Phase: "published", Arguments: &s, Origin: legacyMigrationOrigin,
		Receipt: &api.Receipt{Identity: id, LogicalOwner: p.config.Owner, OperationId: a.OperationID, AcceptedGuarantees: accepted, HistoryRetentionMs: p.config.RetentionMs}}
	if p.executor != nil {
		work, requires, err := p.prepareOrigin(a.OperationID, s, legacyMigrationOrigin)
		if err != nil || len(work) > MaxSpecBytes || !json.Valid(work) {
			return nil, nil, errors.New("executor refused legacy submission")
		}
		j.WorkSpec, j.WorkRequires = work, requires
	}
	data, err := json.Marshal(j)
	if err != nil {
		return nil, nil, err
	}
	// The same decoder recovery uses must accept the journal.
	if _, err := p.decodeJournal(data); err != nil {
		return nil, nil, err
	}
	return data, j, nil
}

func compatible(record *job.Record, j *journal) bool {
	var a, b bytes.Buffer
	return record.ID == j.Receipt.OperationId && record.Kind == j.Arguments.Kind &&
		json.Compact(&a, record.Spec) == nil && json.Compact(&b, j.workSpec()) == nil && bytes.Equal(a.Bytes(), b.Bytes()) &&
		slices.Equal(record.Requires, j.WorkRequires)
}

func migrationProvider(root string, executor Executor, plan migrationRecord, owner, epoch string) *Provider {
	return &Provider{root: root, executor: executor, config: configuration{Version: plan.Storage, Owner: owner, Epoch: epoch, RetentionMs: MinimumRetentionMs, Execution: plan.Execution}}
}

func readPrivate(path string) ([]byte, error) {
	if err := regularFile(path); err != nil {
		return nil, err
	}
	return cas.ReadLimit(path, maxOperationRecordBytes)
}

func evaluateMigration(root string, executor Executor, plan migrationRecord) (LegacyMigration, *migrationRecord, error) {
	var result LegacyMigration
	ownerData, err := readPrivate(filepath.Join(root, "acceptance", "owner.json"))
	if err != nil {
		return result, nil, err
	}
	recordedData, err := readPrivate(filepath.Join(root, "acceptance", legacyMigrationFile))
	if err != nil {
		return result, nil, err
	}
	var recorded *migrationRecord
	if recordedData != nil {
		recorded = new(migrationRecord)
		if err := strict(recordedData, recorded); err != nil || recorded.Version != 1 || recorded.Owner == "" || recorded.Epoch == "" {
			return result, nil, fmt.Errorf("%w: unreadable recorded migration", ErrLegacyMigrationConflict)
		}
		if !sameMapping(*recorded, plan) {
			return result, nil, fmt.Errorf("%w: a different mapping or profile was already recorded", ErrLegacyMigrationConflict)
		}
	}
	if ownerData != nil && recorded == nil {
		return result, nil, fmt.Errorf("%w: root already has a service owner", ErrLegacyMigrationConflict)
	}
	owner, epoch := "pending-owner", "pending-epoch"
	if recorded != nil {
		owner, epoch = recorded.Owner, recorded.Epoch
		result.LogicalOwner, result.HistoryEpoch = owner, epoch
	}
	p := migrationProvider(root, executor, plan, owner, epoch)
	expected := map[string][]byte{}
	journals := map[string]*journal{}
	for _, a := range plan.Assignments {
		data, j, err := p.legacyJournal(a)
		if err != nil {
			continue
		}
		journals[a.OperationID] = j
		expected[filepath.Base(p.requestPath(a.CallerScope, j.Identity))] = data
		result.Converted = append(result.Converted, LegacyConversion{CallerScope: a.CallerScope, Receipt: *j.Receipt})
	}

	if ownerData != nil {
		// Completed migration: verify the retained owner and every journal, write nothing.
		c, err := validateConfiguration(ownerData, "", plan.Storage, plan.Execution, true)
		if err != nil || c.Owner != owner || c.Epoch != epoch || len(journals) != len(plan.Assignments) {
			return LegacyMigration{}, nil, fmt.Errorf("%w: recorded owner does not match migration", ErrLegacyMigrationConflict)
		}
		for name, want := range expected {
			got, err := readPrivate(filepath.Join(p.requests(), name))
			if err != nil || !bytes.Equal(got, want) {
				return LegacyMigration{}, nil, fmt.Errorf("%w: migrated journal missing or changed", ErrLegacyMigrationConflict)
			}
		}
		result.AlreadyMigrated = true
		return result, recorded, nil
	}

	// Journals present before owner.json may only be this recorded migration's.
	requests, err := os.ReadDir(p.requests())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return LegacyMigration{}, nil, err
	}
	for _, entry := range requests {
		name := entry.Name()
		if strings.HasSuffix(name, ".json.lock") {
			continue
		}
		want, ok := expected[name]
		if recorded == nil || !ok {
			return LegacyMigration{}, nil, fmt.Errorf("%w: unowned acceptance history requires explicit migration", ErrLegacyMigrationConflict)
		}
		if got, err := readPrivate(filepath.Join(p.requests(), name)); err != nil || !bytes.Equal(got, want) {
			return LegacyMigration{}, nil, fmt.Errorf("%w: partial migration journal changed", ErrLegacyMigrationConflict)
		}
	}

	inventory, err := InspectLegacyJobs(root)
	if err != nil {
		return LegacyMigration{}, nil, err
	}
	byID := map[string]LegacyAssignment{}
	for _, a := range plan.Assignments {
		byID[a.OperationID] = a
	}
	seen := map[string]bool{}
	for _, item := range inventory {
		seen[item.OperationID] = true
		a, mapped := byID[item.OperationID]
		reason := item.Refusal
		switch {
		case reason != "":
		case !mapped:
			reason = LegacyUnmapped
		case item.RecordSHA256 != a.RecordSHA256:
			reason = LegacyRecordChanged
		default:
			data, record, _ := readLegacyRecord(root, item.OperationID)
			j := journals[item.OperationID]
			if record == nil || digest(data) != a.RecordSHA256 {
				reason = LegacyRecordChanged
			} else if j == nil || !compatible(record, j) {
				reason = LegacyIncompatibleWork
			}
		}
		if reason != "" {
			result.Refused = append(result.Refused, LegacyRefusal{Entry: item.OperationID, Reason: reason})
		}
	}
	for _, a := range plan.Assignments {
		if !seen[a.OperationID] {
			result.Refused = append(result.Refused, LegacyRefusal{Entry: a.OperationID, Reason: LegacyMappedRecordMissing})
		}
	}
	if len(result.Refused) != 0 {
		sort.Slice(result.Refused, func(i, j int) bool { return result.Refused[i].Entry < result.Refused[j].Entry })
		result.Converted = nil
		return result, recorded, ErrLegacyMigrationRefused
	}
	return result, recorded, nil
}

var errLegacyRecordMoved = errors.New("acceptance: legacy record changed during migration")

// AbandonLegacyMigration withdraws an interrupted migration that never wrote
// owner.json, for example after a legacy record changed during commit. No service
// can have opened such a root, so its journals carry no caller-visible receipt.
// It removes only journals naming the recorded owner and epoch, then the record
// of the mapping. Legacy records, work and results are untouched. A completed
// migration is refused with ErrLegacyMigrationConflict.
func AbandonLegacyMigration(root string) error {
	if root == "" {
		return errors.New("acceptance: private root required")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	lease, err := AcquireHost(root)
	if err != nil {
		return err
	}
	defer lease.Close()
	acceptance := filepath.Join(root, "acceptance")
	if owner, err := readPrivate(filepath.Join(acceptance, "owner.json")); err != nil || owner != nil {
		return errors.Join(fmt.Errorf("%w: completed migrations are not abandoned", ErrLegacyMigrationConflict), err)
	}
	path := filepath.Join(acceptance, legacyMigrationFile)
	data, err := readPrivate(path)
	if err != nil || data == nil {
		return err
	}
	var recorded migrationRecord
	if err = strict(data, &recorded); err != nil || recorded.Owner == "" || recorded.Epoch == "" {
		return fmt.Errorf("%w: unreadable recorded migration", ErrLegacyMigrationConflict)
	}
	requests := filepath.Join(acceptance, "requests")
	entries, err := os.ReadDir(requests)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var remove []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".json.lock") {
			continue
		}
		body, err := readPrivate(filepath.Join(requests, name))
		var j journal
		if err != nil || body == nil || strict(body, &j) != nil || j.Receipt == nil || j.Receipt.LogicalOwner != recorded.Owner || j.Identity.HistoryEpoch != recorded.Epoch {
			return fmt.Errorf("%w: acceptance history not written by the recorded migration", ErrLegacyMigrationConflict)
		}
		remove = append(remove, filepath.Join(requests, name))
	}
	for _, name := range remove {
		if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return os.Remove(path)
}

func commitMigration(root string, executor Executor, plan migrationRecord, recorded *migrationRecord) (LegacyMigration, error) {
	acceptance := filepath.Join(root, "acceptance")
	var pinned migrationRecord
	err := cas.ChangeLimit(filepath.Join(acceptance, legacyMigrationFile), maxOperationRecordBytes, func(cur []byte) ([]byte, error) {
		if cur == nil {
			if recorded != nil {
				return nil, fmt.Errorf("%w: recorded migration disappeared", ErrLegacyMigrationConflict)
			}
			pinned = plan
			pinned.Owner, pinned.Epoch = "openabstractions.job/"+job.NewID(), job.NewID()
			return json.Marshal(pinned)
		}
		if err := strict(cur, &pinned); err != nil || !sameMapping(pinned, plan) || pinned.Owner == "" || pinned.Epoch == "" {
			return nil, fmt.Errorf("%w: a different mapping was recorded concurrently", ErrLegacyMigrationConflict)
		}
		return cur, nil
	})
	if err != nil {
		return LegacyMigration{}, err
	}
	if err = migrationPoint("after-plan"); err != nil {
		return LegacyMigration{}, err
	}
	p := migrationProvider(root, executor, plan, pinned.Owner, pinned.Epoch)
	if err = os.MkdirAll(p.requests(), 0700); err != nil {
		return LegacyMigration{}, err
	}
	result := LegacyMigration{LogicalOwner: pinned.Owner, HistoryEpoch: pinned.Epoch}
	for _, a := range plan.Assignments {
		want, j, err := p.legacyJournal(a)
		if err != nil {
			return LegacyMigration{}, err
		}
		// Lock order is record then journal. Recovery takes only the journal lock,
		// and no service host can run while this migration holds the host lease.
		err = cas.ChangeLimit(filepath.Join(root, "jobs", a.OperationID+".json"), maxOperationRecordBytes, func(cur []byte) ([]byte, error) {
			if cur == nil || digest(cur) != a.RecordSHA256 {
				return nil, errLegacyRecordMoved
			}
			record, err := job.Decode(cur)
			if err != nil || !record.State.Terminal() || !compatible(record, j) {
				return nil, errLegacyRecordMoved
			}
			return cur, cas.ChangeLimit(p.requestPath(a.CallerScope, j.Identity), maxOperationRecordBytes, func(existing []byte) ([]byte, error) {
				if existing != nil && !bytes.Equal(existing, want) {
					return nil, fmt.Errorf("%w: journal already names another owner", ErrLegacyMigrationConflict)
				}
				return want, nil
			})
		})
		if err != nil {
			return LegacyMigration{}, err
		}
		if err = migrationPoint("after-journal"); err != nil {
			return LegacyMigration{}, err
		}
		result.Converted = append(result.Converted, LegacyConversion{CallerScope: a.CallerScope, Receipt: *j.Receipt})
	}
	err = cas.Change(filepath.Join(acceptance, "owner.json"), func(cur []byte) ([]byte, error) {
		if cur == nil {
			return json.Marshal(p.config)
		}
		c, err := validateConfiguration(cur, "", plan.Storage, plan.Execution, true)
		if err != nil || c.Owner != p.config.Owner || c.Epoch != p.config.Epoch {
			return nil, fmt.Errorf("%w: owner configured concurrently", ErrLegacyMigrationConflict)
		}
		return cur, nil
	})
	if err != nil {
		return LegacyMigration{}, err
	}
	return result, nil
}
