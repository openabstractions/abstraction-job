package acceptance

import (
	"bytes"
	"fmt"
	"slices"
)

// Evidence is supplied only by an authorized owner transaction, never decoded
// from client claims. This decision helper neither stores evidence nor submits
// effects. A provider must implement the atomicity and fencing in JOB-A3/A4.
type Evidence struct {
	CallerScope      string
	Identity         RequestIdentity
	Arguments        *Submission
	Receipt          *Receipt
	HistoryAvailable bool
	HistoryExpired   bool
	// SealedNonAcceptance means no acceptance occurred AND every delayed request
	// for this identity is fenced. Empty lookup alone must never set this bit.
	SealedNonAcceptance bool
}

func validIdentity(v RequestIdentity) bool { return v.Key != "" && v.HistoryEpoch != "" }

func guaranteeSet(v []string) ([]string, error) {
	out := slices.Clone(v)
	slices.Sort(out)
	for i, s := range out {
		if s == "" || i > 0 && s == out[i-1] {
			return nil, fmt.Errorf("empty or duplicate guarantee")
		}
	}
	return out, nil
}

// ValidateSubmission checks meaning the generated structural codec cannot.
func ValidateSubmission(v Submission) error {
	if !validIdentity(v.Identity) || v.Kind == "" {
		return fmt.Errorf("identity and kind required")
	}
	_, err := guaranteeSet(v.RequiredGuarantees)
	return err
}

func sameArguments(a, b Submission) bool {
	x, ex := guaranteeSet(a.RequiredGuarantees)
	y, ey := guaranteeSet(b.RequiredGuarantees)
	return ex == nil && ey == nil && a.Kind == b.Kind && bytes.Equal(a.Spec, b.Spec) && slices.Equal(x, y)
}

// ValidateResult binds a received receipt to the requested identity and logical
// owner. It does not authenticate the sender or prove its persistence claims.
func ValidateResult(v AcceptanceResult, id RequestIdentity, owner string) error {
	if !validIdentity(id) || owner == "" {
		return fmt.Errorf("identity and owner required")
	}
	if v.Outcome != "accepted" {
		if v.Receipt != nil {
			return fmt.Errorf("receipt on nonaccepted outcome")
		}
		switch v.Outcome {
		case "definitely_not_accepted", "unknown", "key_conflict", "forbidden", "invalid":
			return nil
		default:
			return fmt.Errorf("unknown acceptance outcome")
		}
	}
	r := v.Receipt
	if r == nil || r.Identity != id || r.LogicalOwner != owner || r.OperationId == "" || r.HistoryRetentionMs <= 0 {
		return fmt.Errorf("invalid or mismatched acceptance receipt")
	}
	_, err := guaranteeSet(r.AcceptedGuarantees)
	return err
}

// ReconcileEvidence computes only what retained, authorized evidence proves.
// submitted may be nil for reconciliation after the receipt was lost. The caller
// scope comes from authentication; identity strings are never authority.
func ReconcileEvidence(callerScope, owner string, id RequestIdentity, submitted *Submission, e Evidence) AcceptanceResult {
	result := func(outcome, reason string) AcceptanceResult {
		return AcceptanceResult{Outcome: outcome, Reason: reason}
	}
	if callerScope == "" || callerScope != e.CallerScope {
		return result("forbidden", "authenticated scope unavailable or not authorized")
	}
	if !validIdentity(id) || owner == "" {
		return result("invalid", "identity and owner required")
	}
	if submitted != nil && (ValidateSubmission(*submitted) != nil || submitted.Identity != id) {
		return result("invalid", "invalid submission")
	}
	if e.Identity != id || !e.HistoryAvailable || e.HistoryExpired {
		return result("unknown", "history unavailable or expired")
	}
	if e.Receipt != nil {
		if e.SealedNonAcceptance {
			return result("unknown", "contradictory owner evidence")
		}
		r := *e.Receipt
		r.AcceptedGuarantees = slices.Clone(r.AcceptedGuarantees)
		v := AcceptanceResult{Outcome: "accepted", Receipt: &r, Reason: "original acceptance"}
		if ValidateResult(v, id, owner) != nil {
			return result("unknown", "invalid owner receipt")
		}
		if e.Arguments != nil {
			if e.Arguments.Identity != id || ValidateSubmission(*e.Arguments) != nil {
				return result("unknown", "invalid retained arguments")
			}
			for _, g := range e.Arguments.RequiredGuarantees {
				if !slices.Contains(r.AcceptedGuarantees, g) {
					return result("unknown", "receipt weakens required guarantees")
				}
			}
		}
		if submitted != nil {
			if e.Arguments == nil {
				return result("unknown", "argument evidence unavailable")
			}
			if !sameArguments(*submitted, *e.Arguments) {
				return result("key_conflict", "identity reused with different arguments")
			}
		}
		return v
	}
	if e.SealedNonAcceptance {
		return result("definitely_not_accepted", "identity sealed against acceptance")
	}
	return result("unknown", "no authoritative sealed acceptance history")
}

// CanResolveFresh must only consume an authenticated, semantically validated
// owner response. Transport errors and local cancellation never produce one.
func CanResolveFresh(v AcceptanceResult) bool {
	return v.Outcome == "definitely_not_accepted" && v.Receipt == nil
}
