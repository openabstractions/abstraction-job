package acceptanceprovider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

// Journals keep the private on-disk shape they had before request attempts
// existed. A zero attempt and an unlost result are omitted, so a journal for an
// original request is byte-compatible with providers that decode journals
// strictly and predate attempts. Only a retry attempt or a recorded JOB-A10 loss
// writes a field such a provider refuses, and it refuses that one journal.
type journalIdentity struct {
	Key          string
	HistoryEpoch string
	Attempt      int64 `json:",omitempty"`
}

type journalSubmission struct {
	Identity           journalIdentity
	Kind               string
	Spec               []byte
	RequiredGuarantees []string
}

type journalReceipt struct {
	Identity           journalIdentity
	LogicalOwner       string
	OperationId        string
	AcceptedGuarantees []string
	HistoryRetentionMs int64
}

type journalDocument struct {
	Version      int
	Scope        string
	Identity     journalIdentity
	Phase        string
	Arguments    *journalSubmission
	Receipt      *journalReceipt
	Reason       string
	WorkSpec     []byte   `json:",omitempty"`
	WorkRequires []string `json:",omitempty"`
	Origin       string   `json:",omitempty"`
	ResultLost   bool     `json:",omitempty"`
}

func toJournalIdentity(id api.RequestIdentity) journalIdentity {
	return journalIdentity{Key: id.Key, HistoryEpoch: id.HistoryEpoch, Attempt: id.Attempt}
}

func (id journalIdentity) api() api.RequestIdentity {
	return api.RequestIdentity{Key: id.Key, HistoryEpoch: id.HistoryEpoch, Attempt: id.Attempt}
}

func (j journal) MarshalJSON() ([]byte, error) {
	doc := journalDocument{Version: j.Version, Scope: j.Scope, Identity: toJournalIdentity(j.Identity), Phase: j.Phase, Reason: j.Reason,
		WorkSpec: j.WorkSpec, WorkRequires: j.WorkRequires, Origin: j.Origin, ResultLost: j.ResultLost}
	if s := j.Arguments; s != nil {
		doc.Arguments = &journalSubmission{Identity: toJournalIdentity(s.Identity), Kind: s.Kind, Spec: s.Spec, RequiredGuarantees: s.RequiredGuarantees}
	}
	if r := j.Receipt; r != nil {
		doc.Receipt = &journalReceipt{Identity: toJournalIdentity(r.Identity), LogicalOwner: r.LogicalOwner, OperationId: r.OperationId,
			AcceptedGuarantees: r.AcceptedGuarantees, HistoryRetentionMs: r.HistoryRetentionMs}
	}
	return json.Marshal(doc)
}

// UnmarshalJSON refuses unknown fields itself: a custom decoder does not inherit
// the caller's DisallowUnknownFields setting.
func (j *journal) UnmarshalJSON(b []byte) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	var doc journalDocument
	if err := d.Decode(&doc); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("acceptance: trailing journal data")
	}
	*j = journal{Version: doc.Version, Scope: doc.Scope, Identity: doc.Identity.api(), Phase: doc.Phase, Reason: doc.Reason,
		WorkSpec: doc.WorkSpec, WorkRequires: doc.WorkRequires, Origin: doc.Origin, ResultLost: doc.ResultLost}
	if s := doc.Arguments; s != nil {
		j.Arguments = &api.Submission{Identity: s.Identity.api(), Kind: s.Kind, Spec: s.Spec, RequiredGuarantees: s.RequiredGuarantees}
	}
	if r := doc.Receipt; r != nil {
		j.Receipt = &api.Receipt{Identity: r.Identity.api(), LogicalOwner: r.LogicalOwner, OperationId: r.OperationId,
			AcceptedGuarantees: r.AcceptedGuarantees, HistoryRetentionMs: r.HistoryRetentionMs}
	}
	return nil
}
