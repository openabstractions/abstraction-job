package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

// This is a protocol fixture with retained evidence, not a durable provider.
type acceptanceFixture struct {
	evidence                   Evidence
	submissions, cancellations int
}

func (f *acceptanceFixture) GetHistoryWindow() (HistoryWindow, error) {
	return HistoryWindow{LogicalOwner: "owner", HistoryEpoch: "epoch", MinimumRetentionMs: 60000}, nil
}
func (f *acceptanceFixture) Submit(s Submission) (AcceptanceResult, error) {
	f.submissions++
	return ReconcileEvidence("caller", "owner", s.Identity, &s, f.evidence), nil
}
func (f *acceptanceFixture) Reconcile(id RequestIdentity) (AcceptanceResult, error) {
	return ReconcileEvidence("caller", "owner", id, nil, f.evidence), nil
}
func (f *acceptanceFixture) CancelWork(id RequestIdentity) (CancellationResult, error) {
	f.cancellations++
	return CancellationResult{Outcome: "requested"}, nil
}

type loseReply struct {
	dispatcher *RecoverableAcceptanceDispatcher
	lost       bool
}

func (l *loseReply) ExchangeFrame(frame []byte) ([]byte, error) {
	reply, err := l.dispatcher.ExchangeFrame(frame)
	if !l.lost {
		l.lost = true
		return nil, errors.New("reply lost after handler returned")
	}
	return reply, err
}

func TestLostReplyThroughGeneratedService(t *testing.T) {
	id := RequestIdentity{Key: "saved-before-send", HistoryEpoch: "epoch"}
	args := Submission{Identity: id, Kind: "download", Spec: []byte("opaque"), RequiredGuarantees: []string{"reconcile@1"}}
	f := &acceptanceFixture{evidence: Evidence{CallerScope: "caller", Identity: id, Arguments: &args, HistoryAvailable: true, Receipt: &Receipt{Identity: id, LogicalOwner: "owner", OperationId: "original-operation", AcceptedGuarantees: []string{"reconcile@1"}, HistoryRetentionMs: 60000}}}
	transport := &loseReply{dispatcher: &RecoverableAcceptanceDispatcher{Handler: f}}
	client := NewRecoverableAcceptanceClient(transport)
	if _, err := client.Submit(args); err == nil {
		t.Fatal("expected lost reply")
	}
	// A new client models reconnect/restart with the retained pre-send identity.
	client = NewRecoverableAcceptanceClient(transport)
	got, err := client.Reconcile(id)
	if err != nil || got.Receipt == nil || got.Receipt.OperationId != "original-operation" {
		t.Fatalf("recovery: %+v %v", got, err)
	}
	if f.submissions != 1 || f.cancellations != 0 {
		t.Fatal("reconciliation submitted or cancelled work")
	}
	cancelled, err := client.CancelWork(id)
	if err != nil || cancelled.Outcome != "requested" || f.cancellations != 1 {
		t.Fatalf("explicit cancel: %+v %v", cancelled, err)
	}
}

func TestAcceptanceCorpus(t *testing.T) {
	data, err := os.ReadFile("../../../../testdata/acceptance.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, Want, Changed                                       string
		History, Expired, Receipt, Sealed, Submit, Reorder, Fresh bool
		CancelWait                                                bool `json:"cancel_wait"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			id := RequestIdentity{Key: "stable-key", HistoryEpoch: "owner-epoch-1"}
			args := Submission{Identity: id, Kind: "download", Spec: []byte(`{"source":"artifact"}`), RequiredGuarantees: []string{"caller-exit@1", "reconcile@1"}}
			e := Evidence{CallerScope: "authenticated-alice", Identity: id, Arguments: &args, HistoryAvailable: c.History, HistoryExpired: c.Expired, SealedNonAcceptance: c.Sealed}
			if c.Receipt {
				e.Receipt = &Receipt{Identity: id, LogicalOwner: "owner-1", OperationId: "operation-1", AcceptedGuarantees: []string{"caller-exit@1", "reconcile@1"}, HistoryRetentionMs: 60000}
			}
			caller, owner := "authenticated-alice", "owner-1"
			submission := args
			if c.Reorder {
				submission.RequiredGuarantees = []string{"reconcile@1", "caller-exit@1"}
			}
			switch c.Changed {
			case "spec":
				submission.Spec = []byte(`{"source":"other"}`)
			case "kind":
				submission.Kind = "other"
			case "guarantees":
				submission.RequiredGuarantees = []string{"reconcile@1"}
			case "identity":
				id.Key = "lost-key-replacement"
			case "caller":
				caller = "authenticated-bob"
			case "owner":
				owner = "owner-2"
			}
			var submitted *Submission
			if c.Submit {
				submitted = &submission
			}
			if c.CancelWait {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if ctx.Err() == nil {
					t.Fatal("wait still active")
				}
			}
			got := ReconcileEvidence(caller, owner, id, submitted, e)
			if got.Outcome != c.Want || CanResolveFresh(got) != c.Fresh {
				t.Fatalf("got %+v fresh=%v; want %s fresh=%v", got, CanResolveFresh(got), c.Want, c.Fresh)
			}
			if err := ValidateResult(got, id, owner); err != nil {
				t.Fatal(err)
			}
			// Exercise the generated document codec on every semantic result.
			round, err := Decode(Encode(&got))
			if err != nil || round.Outcome != got.Outcome {
				t.Fatalf("generated roundtrip: %+v %v", round, err)
			}
			if got.Receipt != nil {
				if got.Receipt.OperationId != "operation-1" {
					t.Fatal("duplicate operation")
				}
				got.Receipt.AcceptedGuarantees[0] = "mutated"
				if e.Receipt.AcceptedGuarantees[0] == "mutated" {
					t.Fatal("shared owner evidence escaped")
				}
			}
		})
	}
}

func TestAcceptanceValidation(t *testing.T) {
	id := RequestIdentity{Key: "key", HistoryEpoch: "epoch"}
	for _, v := range []AcceptanceResult{
		{Outcome: "accepted"}, {Outcome: "new-future-outcome"},
		{Outcome: "unknown", Receipt: &Receipt{}},
		{Outcome: "accepted", Receipt: &Receipt{Identity: id, LogicalOwner: "owner", OperationId: "op", HistoryRetentionMs: 0}},
	} {
		if ValidateResult(v, id, "owner") == nil {
			t.Fatalf("accepted invalid result %+v", v)
		}
	}
	if CanResolveFresh(AcceptanceResult{}) || CanResolveFresh(AcceptanceResult{Outcome: "unknown"}) {
		t.Fatal("uncertainty permits fresh resolution")
	}
	if ValidateSubmission(Submission{Identity: id, Kind: "download", RequiredGuarantees: []string{"same", "same"}}) == nil {
		t.Fatal("duplicate guarantees accepted")
	}
}

func TestReconcileValidatesRetainedArgumentsWithoutResubmission(t *testing.T) {
	id := RequestIdentity{Key: "key", HistoryEpoch: "epoch"}
	for _, name := range []string{"missing-guarantee", "wrong-identity", "empty-kind", "duplicate-guarantee", "arguments-absent"} {
		t.Run(name, func(t *testing.T) {
			original := Submission{Identity: id, Kind: "download", RequiredGuarantees: []string{"durable@1"}}
			retained := original
			e := Evidence{CallerScope: "caller", Identity: id, HistoryAvailable: true, Arguments: &retained,
				Receipt: &Receipt{Identity: id, LogicalOwner: "owner", OperationId: "operation", AcceptedGuarantees: []string{"durable@1"}, HistoryRetentionMs: 1000}}
			switch name {
			case "missing-guarantee":
				e.Receipt.AcceptedGuarantees = nil
			case "wrong-identity":
				retained.Identity.Key = "different"
			case "empty-kind":
				retained.Kind = ""
			case "duplicate-guarantee":
				retained.RequiredGuarantees = []string{"durable@1", "durable@1"}
			case "arguments-absent":
				e.Arguments = nil
			}
			for _, submitted := range []*Submission{nil, &original} {
				want := "unknown"
				if name == "arguments-absent" && submitted == nil {
					want = "accepted"
				}
				got := ReconcileEvidence("caller", "owner", id, submitted, e)
				if got.Outcome != want || CanResolveFresh(got) {
					t.Fatalf("resubmitted=%v: got %+v, want %s", submitted != nil, got, want)
				}
			}
		})
	}
}
