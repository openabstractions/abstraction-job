package acceptanceprovider

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const testExecutionPromise = "example.execution/recovery@1"

type promisedExecutor struct {
	testExecutor
	requires []string
	prepares *int
}

func (e promisedExecutor) ExecutionGuarantees() []string { return []string{testExecutionPromise} }
func (e promisedExecutor) PrepareWithGuarantees(id, kind string, spec []byte, required []string) ([]byte, []string, error) {
	if e.prepares != nil {
		*e.prepares++
	}
	if !slices.Contains(required, testExecutionPromise) {
		return nil, nil, errors.New("missing promise")
	}
	work, err := e.Prepare(id, kind, spec)
	return work, e.requires, err
}

func TestExecutionGuaranteesPersistThroughLostReply(t *testing.T) {
	root := t.TempDir()
	e := promisedExecutor{testExecutor: testExecutor{profile: "promised-v1"}, requires: []string{testExecutionPromise}}
	p, err := OpenWithExecutor(root, "owner", e)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(p.SupportedGuarantees(), testExecutionPromise) {
		t.Fatal("promise absent from discovery")
	}
	s := submission(p, "required")
	s.RequiredGuarantees = append(s.RequiredGuarantees, testExecutionPromise)
	p.fault = func(point string) error {
		if point == "after-job" {
			return errors.New("lost reply")
		}
		return nil
	}
	r, err := p.Bind("alice").Submit(s)
	if err != nil || r.Outcome != "unknown" {
		t.Fatalf("lost reply: %+v %v", r, err)
	}
	p, err = OpenWithExecutor(root, "owner", e)
	if err != nil {
		t.Fatal(err)
	}
	receipt := accept(t, p.Bind("alice"), s)
	if !slices.Contains(receipt.AcceptedGuarantees, testExecutionPromise) {
		t.Fatal("receipt lost execution guarantee")
	}
	record, err := p.store.Load(receipt.OperationId)
	if err != nil || !slices.Equal(record.Requires, e.requires) || jobs(t, root) != 1 {
		t.Fatalf("prepared requirements lost: %+v %v", record, err)
	}
	if _, err := OpenWithExecutor(root, "owner", e.testExecutor); err == nil {
		t.Fatal("executor downgrade opened guaranteed work")
	}
	changed := e
	changed.requires = nil
	if _, err := OpenWithExecutor(root, "owner", changed); err == nil {
		t.Fatal("changed preparation weakened retained requirements")
	}
	plain := submission(p, "plain")
	plainReceipt := accept(t, p.Bind("alice"), plain)
	if slices.Contains(plainReceipt.AcceptedGuarantees, testExecutionPromise) {
		t.Fatal("unrequested promise accepted without preparation")
	}
	path := filepath.Join(root, "jobs", receipt.OperationId+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// An otherwise valid operation cannot shed its accepted execution requirements.
	record.Requires = nil
	weakened, err := record.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, weakened, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenWithExecutor(root, "owner", e); err == nil {
		t.Fatal("weakened operation opened")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(weakened) {
		t.Fatal("recovery rewrote weakened operation")
	}
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestUnsupportedExecutionGuaranteeSealsBeforePreparation(t *testing.T) {
	root := t.TempDir()
	count := 0
	e := promisedExecutor{testExecutor: testExecutor{profile: "promised-v1"}, prepares: &count}
	p, err := OpenWithExecutor(root, "owner", e)
	if err != nil {
		t.Fatal(err)
	}
	s := submission(p, "unsupported")
	s.RequiredGuarantees = []string{"example.execution/unsupported@1"}
	r, err := p.Bind("alice").Submit(s)
	if err != nil || r.Outcome != "definitely_not_accepted" || count != 0 || jobs(t, root) != 0 {
		t.Fatalf("unsupported requirement: %+v %v prepares=%d", r, err, count)
	}
	s.RequiredGuarantees = []string{testExecutionPromise}
	r, err = p.Bind("alice").Submit(s)
	if err != nil || r.Outcome != "definitely_not_accepted" || count != 0 {
		t.Fatalf("sealed identity became eligible: %+v %v", r, err)
	}
}
