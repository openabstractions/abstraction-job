package acceptance

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
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
	// DecisionUnavailable means a required service-owned policy decision could
	// not be obtained, so no evidence was consulted or written [JOB-A9].
	DecisionUnavailable bool
}

func validIdentity(v RequestIdentity) bool {
	return v.Key != "" && v.HistoryEpoch != "" && v.Attempt >= 0
}

// AttemptEvidence is trusted owner evidence for the attempt immediately before
// a requested attempt, under the same authenticated scope, key and epoch
// [JOB-A7]. It is consulted only when the requested attempt has no journal.
type AttemptEvidence struct {
	// Previous is "absent", "sealed" or "accepted". Other values are contradictory.
	Previous string
	// State is the accepted previous attempt's operation state, empty when unavailable.
	State string
	// LatestAccepted holds the arguments of the latest accepted earlier attempt.
	LatestAccepted *Submission
}

// AttemptEligibility decides whether id may be accepted or sealed. An empty
// outcome means eligible. A nonempty outcome must be returned without writing
// acceptance or seal evidence for id, so a later presentation can succeed.
func AttemptEligibility(id RequestIdentity, submitted *Submission, e AttemptEvidence) AcceptanceResult {
	refuse := func(outcome AcceptanceOutcome, reason string) AcceptanceResult {
		return AcceptanceResult{Outcome: outcome, Reason: reason}
	}
	if id.Attempt < 0 {
		return refuse(AcceptanceOutcomeInvalid, "attempt must be nonnegative")
	}
	if id.Attempt == 0 {
		return AcceptanceResult{}
	}
	switch e.Previous {
	case "absent":
		return refuse(AcceptanceOutcomeInvalid, "previous attempt absent")
	case "sealed":
	case "accepted":
		switch e.State {
		case "failed":
		case "":
			return refuse(AcceptanceOutcomeUnknown, "previous attempt state unavailable")
		case "pending", "running", "transferred", "complete", "cancelled":
			return refuse(AcceptanceOutcomeInvalid, "previous attempt has not failed terminally")
		default:
			return refuse(AcceptanceOutcomeUnknown, "contradictory previous attempt state")
		}
	default:
		return refuse(AcceptanceOutcomeUnknown, "contradictory previous attempt evidence")
	}
	if submitted == nil {
		return AcceptanceResult{}
	}
	if e.LatestAccepted == nil {
		if e.Previous == "accepted" {
			return refuse(AcceptanceOutcomeUnknown, "previous attempt arguments unavailable")
		}
		return AcceptanceResult{}
	}
	if !sameArguments(*submitted, *e.LatestAccepted) {
		return refuse(AcceptanceOutcomeKeyConflict, "retry arguments differ from the accepted attempt")
	}
	return AcceptanceResult{}
}

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
	if _, err := NormalizeLabel(v.Label); err != nil {
		return err
	}
	_, err := guaranteeSet(v.RequiredGuarantees)
	return err
}

// MaxLabelBytes is the largest display label in UTF-8 bytes [JOB-A12].
const MaxLabelBytes = 256

// NormalizeLabel trims a display label and checks its limits [JOB-A12]. The
// result is empty when the label is absent. A label that is not valid UTF-8,
// is longer than MaxLabelBytes after trimming, or holds a code point below
// U+0020, U+007F, U+2028 or U+2029 is refused.
func NormalizeLabel(label string) (string, error) {
	if !utf8.ValidString(label) {
		return "", fmt.Errorf("label is not valid UTF-8")
	}
	label = strings.TrimSpace(label)
	if len(label) > MaxLabelBytes {
		return "", fmt.Errorf("label exceeds %d bytes", MaxLabelBytes)
	}
	for _, r := range label {
		if r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029 {
			return "", fmt.Errorf("label must be one line of printable text")
		}
	}
	return label, nil
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
	if v.Outcome != AcceptanceOutcomeAccepted {
		if v.Receipt != nil {
			return fmt.Errorf("receipt on nonaccepted outcome")
		}
		switch v.Outcome {
		case AcceptanceOutcomeDefinitelyNotAccepted, AcceptanceOutcomeUnknown, AcceptanceOutcomeKeyConflict, AcceptanceOutcomeForbidden, AcceptanceOutcomeInvalid, AcceptanceOutcomeUnavailable:
			return nil
		default:
			return fmt.Errorf("unknown acceptance outcome")
		}
	}
	r := v.Receipt
	if r == nil || r.Identity != id || r.LogicalOwner != owner || r.OperationID == "" || r.HistoryRetentionMs <= 0 {
		return fmt.Errorf("invalid or mismatched acceptance receipt")
	}
	_, err := guaranteeSet(r.AcceptedGuarantees)
	return err
}

// ReconcileEvidence computes only what retained, authorized evidence proves.
// submitted may be nil for reconciliation after the receipt was lost. The caller
// scope comes from authentication; identity strings are never authority.
func ReconcileEvidence(callerScope, owner string, id RequestIdentity, submitted *Submission, e Evidence) AcceptanceResult {
	result := func(outcome AcceptanceOutcome, reason string) AcceptanceResult {
		return AcceptanceResult{Outcome: outcome, Reason: reason}
	}
	if callerScope == "" || callerScope != e.CallerScope {
		return result(AcceptanceOutcomeForbidden, "authenticated scope unavailable or not authorized")
	}
	if e.DecisionUnavailable {
		return result(AcceptanceOutcomeUnavailable, "policy decision unavailable; the same identity may be presented again")
	}
	if !validIdentity(id) || owner == "" {
		return result(AcceptanceOutcomeInvalid, "identity and owner required")
	}
	if submitted != nil && (ValidateSubmission(*submitted) != nil || submitted.Identity != id) {
		return result(AcceptanceOutcomeInvalid, "invalid submission")
	}
	if e.Identity != id || !e.HistoryAvailable || e.HistoryExpired {
		return result(AcceptanceOutcomeUnknown, "history unavailable or expired")
	}
	if e.Receipt != nil {
		if e.SealedNonAcceptance {
			return result(AcceptanceOutcomeUnknown, "contradictory owner evidence")
		}
		r := *e.Receipt
		r.AcceptedGuarantees = slices.Clone(r.AcceptedGuarantees)
		v := AcceptanceResult{Outcome: AcceptanceOutcomeAccepted, Receipt: &r, Reason: "original acceptance"}
		if ValidateResult(v, id, owner) != nil {
			return result(AcceptanceOutcomeUnknown, "invalid owner receipt")
		}
		if e.Arguments != nil {
			if e.Arguments.Identity != id || ValidateSubmission(*e.Arguments) != nil {
				return result(AcceptanceOutcomeUnknown, "invalid retained arguments")
			}
			for _, g := range e.Arguments.RequiredGuarantees {
				if !slices.Contains(r.AcceptedGuarantees, g) {
					return result(AcceptanceOutcomeUnknown, "receipt weakens required guarantees")
				}
			}
		}
		if submitted != nil {
			if e.Arguments == nil {
				return result(AcceptanceOutcomeUnknown, "argument evidence unavailable")
			}
			if !sameArguments(*submitted, *e.Arguments) {
				return result(AcceptanceOutcomeKeyConflict, "identity reused with different arguments")
			}
		}
		return v
	}
	if e.SealedNonAcceptance {
		return result(AcceptanceOutcomeDefinitelyNotAccepted, "identity sealed against acceptance")
	}
	return result(AcceptanceOutcomeUnknown, "no authoritative sealed acceptance history")
}

// CanResolveFresh must only consume an authenticated, semantically validated
// owner response. Transport errors and local cancellation never produce one.
func CanResolveFresh(v AcceptanceResult) bool {
	return v.Outcome == AcceptanceOutcomeDefinitelyNotAccepted && v.Receipt == nil
}
