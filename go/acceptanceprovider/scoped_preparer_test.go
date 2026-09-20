package acceptanceprovider

import (
	"encoding/json"
	"errors"
	"testing"
)

// scopedExecutor records the submitting scope in the work it prepares.
type scopedExecutor struct{ testExecutor }

func (e scopedExecutor) PrepareScoped(scope, id, kind string, spec []byte, required []string) ([]byte, []string, error) {
	if len(required) != 0 {
		return nil, nil, errors.New("unsupported guarantee")
	}
	work, err := json.Marshal(map[string]string{"scope": scope, "private_destination": e.prefix + id})
	return work, nil, err
}

// TestScopedPreparerReceivesTheSubmittingScope proves new work and recovery
// both prepare with the journaled caller scope, so recovery reproduces it.
func TestScopedPreparerReceivesTheSubmittingScope(t *testing.T) {
	root := t.TempDir()
	executor := scopedExecutor{testExecutor{profile: "scoped-v1", prefix: "private/"}}
	p, err := OpenWithExecutor(root, "logical-owner", executor)
	if err != nil {
		t.Fatal(err)
	}
	s := submission(p, "scoped")
	p.fault = func(at string) error {
		if at == "after-journal" {
			return errors.New("interrupted")
		}
		return nil
	}
	if result, err := p.Bind("alice").Submit(s); err != nil || result.Outcome.String() != "unknown" {
		t.Fatalf("fault response: %+v %v", result, err)
	}
	p, err = OpenWithExecutor(root, "logical-owner", executor)
	if err != nil {
		t.Fatalf("recovery with the journaled scope: %v", err)
	}
	r := accept(t, p.Bind("alice"), s)
	rec, err := p.store.Load(r.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	var work map[string]string
	if err := json.Unmarshal(rec.Spec, &work); err != nil || work["scope"] != "alice" {
		t.Fatalf("prepared work %s: %v", rec.Spec, err)
	}
	other := submission(p, "scoped-bob")
	b := accept(t, p.Bind("bob"), other)
	rec, err = p.store.Load(b.OperationID)
	if err != nil || json.Unmarshal(rec.Spec, &work) != nil || work["scope"] != "bob" {
		t.Fatalf("second scope: %s %v", rec.Spec, err)
	}
}
