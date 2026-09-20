package acceptanceprovider

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

// labelExecutor derives a label from the spec text, as a kind's provider would.
type labelExecutor struct {
	testExecutor
	derived string
}

func (e labelExecutor) DeriveLabel(kind string, spec []byte) string { return e.derived }

// labelOf observes one identity and returns its stored label [JOB-A12].
func labelOf(t *testing.T, p *Provider, scope string, id api.RequestIdentity) (string, bool) {
	t.Helper()
	observed, err := p.BindOperations(scope).ObserveWork(id)
	if err != nil || observed.Outcome.String() != "observed" || observed.Snapshot == nil {
		t.Fatalf("observe: %+v %v", observed, err)
	}
	return observed.Snapshot.Label, observed.Snapshot.LabelDerived
}

func journalFiles(t *testing.T, p *Provider) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(p.requests(), "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func TestCallerLabelIsStoredOnceOutsideEquality(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	c := p.Bind("alice")
	s := submission(p, "labelled")
	s.Label = "  org/tiny@abc · unet/model.safetensors\t"
	r := accept(t, c, s)
	if label, derived := labelOf(t, p, "alice", s.Identity); label != "org/tiny@abc · unet/model.safetensors" || derived {
		t.Fatalf("stored label %q derived=%v", label, derived)
	}

	// A duplicate with a different label is the same submission (JOB-A2) and
	// keeps the stored label.
	relabelled := s
	relabelled.Label = "something else"
	if again := accept(t, c, relabelled); again.OperationID != r.OperationID {
		t.Fatal("a different label created a second operation")
	}
	unlabelled := s
	unlabelled.Label = ""
	if again := accept(t, c, unlabelled); again.OperationID != r.OperationID {
		t.Fatal("an absent label created a second operation")
	}
	if label, _ := labelOf(t, p, "alice", s.Identity); label != "org/tiny@abc · unet/model.safetensors" {
		t.Fatalf("replay changed the label to %q", label)
	}

	// The label survives a restart, and inventory reports the same one.
	p = openTest(t, root)
	if label, derived := labelOf(t, p, "alice", s.Identity); label != "org/tiny@abc · unet/model.safetensors" || derived {
		t.Fatalf("restart label %q derived=%v", label, derived)
	}
	page, err := p.BindInventory("alice").ListWork("", 8)
	if err != nil || page.Outcome.String() != "page" || len(page.Snapshots) != 1 || page.Snapshots[0].Label != "org/tiny@abc · unet/model.safetensors" || page.Snapshots[0].LabelDerived {
		t.Fatalf("inventory: %+v %v", page, err)
	}
	p.CloseInventory()

	// The label is stored beside the arguments, never inside them, and the
	// header names the storage feature before the journal needed it.
	j, _, err := p.readJournal("alice", s.Identity)
	if err != nil || j.Arguments.Label != "" || j.Label == "" {
		t.Fatalf("journal: %+v %v", j, err)
	}
	owner, err := os.ReadFile(filepath.Join(root, "acceptance", "owner.json"))
	if err != nil || !bytes.Contains(owner, []byte(StorageFeatureLabels)) {
		t.Fatalf("owner header lacks the labels feature: %s %v", owner, err)
	}
}

func TestInvalidLabelJournalsNothing(t *testing.T) {
	p := openTest(t, t.TempDir())
	c := p.Bind("alice")
	for _, label := range []string{"line\nbreak", "nul\x00", "del\x7f", "line separator", "paragraph separator", strings.Repeat("x", api.MaxLabelBytes+1), "bad\xff"} {
		s := submission(p, "invalid-label")
		s.Label = label
		v, err := c.Submit(s)
		if err != nil || v.Outcome.String() != "invalid" || v.Receipt != nil {
			t.Fatalf("label %q: %+v %v", label, v, err)
		}
	}
	if files := journalFiles(t, p); len(files) != 0 {
		t.Fatalf("an invalid label journaled %d records", len(files))
	}
	// Nothing was sealed: the same identity with a valid label is accepted.
	s := submission(p, "invalid-label")
	s.Label = strings.Repeat("x", api.MaxLabelBytes)
	accept(t, c, s)
	if label, _ := labelOf(t, p, "alice", s.Identity); label != s.Label {
		t.Fatalf("label %q", label)
	}
}

func TestDerivedLabelAndItsAbsence(t *testing.T) {
	root := t.TempDir()
	executor := labelExecutor{testExecutor: testExecutor{profile: "test-v1", prefix: "private/"}, derived: "host · model.bin"}
	p, err := OpenWithExecutor(root, "logical-owner", executor)
	if err != nil {
		t.Fatal(err)
	}
	c := p.Bind("alice")
	derived := submission(p, "derived")
	accept(t, c, derived)
	if label, isDerived := labelOf(t, p, "alice", derived.Identity); label != "host · model.bin" || !isDerived {
		t.Fatalf("derived label %q derived=%v", label, isDerived)
	}
	caller := submission(p, "caller")
	caller.Label = "mine"
	accept(t, c, caller)
	if label, isDerived := labelOf(t, p, "alice", caller.Identity); label != "mine" || isDerived {
		t.Fatalf("caller label %q derived=%v", label, isDerived)
	}

	// A restart under the same profile keeps the derived label as stored, even
	// when derivation would now answer differently.
	executor.derived = "changed"
	p, err = OpenWithExecutor(root, "logical-owner", executor)
	if err != nil {
		t.Fatal(err)
	}
	if label, isDerived := labelOf(t, p, "alice", derived.Identity); label != "host · model.bin" || !isDerived {
		t.Fatalf("restart derived label %q derived=%v", label, isDerived)
	}

	// A derivation outside the limits leaves the label absent.
	executor.derived = "two\nlines"
	p, err = OpenWithExecutor(root, "logical-owner", executor)
	if err != nil {
		t.Fatal(err)
	}
	absent := submission(p, "absent")
	accept(t, p.Bind("alice"), absent)
	if label, isDerived := labelOf(t, p, "alice", absent.Identity); label != "" || isDerived {
		t.Fatalf("invalid derivation stored %q derived=%v", label, isDerived)
	}
}

func TestUnlabelledJournalUsesNoLabelFeature(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	s := submission(p, "plain")
	accept(t, p.Bind("alice"), s)
	for _, path := range journalFiles(t, p) {
		data, err := os.ReadFile(path)
		if err != nil || bytes.Contains(data, []byte("Label")) {
			t.Fatalf("unlabelled journal carries a label field: %s %v", data, err)
		}
	}
	owner, err := os.ReadFile(filepath.Join(root, "acceptance", "owner.json"))
	if err != nil || bytes.Contains(owner, []byte(StorageFeatureLabels)) {
		t.Fatalf("owner header: %s %v", owner, err)
	}
	// An older validator refuses a labelled store by the feature's name.
	labelled := submission(p, "labelled")
	labelled.Label = "x"
	accept(t, p.Bind("alice"), labelled)
	owner, err = os.ReadFile(filepath.Join(root, "acceptance", "owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	older := slices.DeleteFunc(SupportedStorageFeatures(), func(f string) bool { return f == StorageFeatureLabels })
	_, err = validateConfigurationFeatures(owner, "logical-owner", p.config.Version, p.config.Execution, false, older)
	if err == nil || !strings.Contains(err.Error(), StorageFeatureLabels) {
		t.Fatalf("older validator: %v", err)
	}
}
