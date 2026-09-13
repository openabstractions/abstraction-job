package acceptanceprovider

import (
	"slices"
	"testing"

	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

func TestAdmissionGuaranteeVocabulary(t *testing.T) {
	// Keep exported Go compile-time constants source-compatible while the
	// definition's generated roster supplies identifiers to every language.
	if !slices.Equal(Guarantees(), api.AdmissionGuarantees) {
		t.Fatalf("provider guarantees %q differ from definition %q", Guarantees(), api.AdmissionGuarantees)
	}
	const oldAPI = GuaranteeCallerExit + GuaranteeServiceRestart + GuaranteeReconciliation
	if oldAPI == "" {
		t.Fatal("missing admission guarantees")
	}
}
