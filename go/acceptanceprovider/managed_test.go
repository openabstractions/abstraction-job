package acceptanceprovider

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedCompetingOpenersAndRestartKeepIdentity(t *testing.T) {
	root := t.TempDir()
	executor := testExecutor{profile: "managed-test-v1"}
	type result struct {
		p   *Provider
		err error
	}
	start, results := make(chan struct{}), make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() { <-start; p, err := OpenManaged(root, executor); results <- result{p, err} }()
	}
	close(start)
	a, b := <-results, <-results
	if a.err != nil || b.err != nil {
		t.Fatal(a.err, b.err)
	}
	if !strings.HasPrefix(a.p.LogicalOwner(), "openabstractions.job/") || a.p.LogicalOwner() != b.p.LogicalOwner() || a.p.config.Epoch != b.p.config.Epoch {
		t.Fatal("competing openers did not retain one identity", a.p.config, b.p.config)
	}
	s := submission(a.p, "retained")
	accepted, err := a.p.Bind("caller").Submit(s)
	if err != nil || accepted.Outcome != "accepted" {
		t.Fatal(accepted, err)
	}
	reopened, err := OpenManaged(root, executor)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.LogicalOwner() != a.p.LogicalOwner() || reopened.config.Epoch != a.p.config.Epoch {
		t.Fatal("restart changed identity")
	}
	reconciled, err := reopened.Bind("caller").Reconcile(s.Identity)
	if err != nil || reconciled.Receipt == nil || reconciled.Receipt.OperationId != accepted.Receipt.OperationId || reconciled.Receipt.LogicalOwner != reopened.LogicalOwner() {
		t.Fatal(reconciled, err)
	}
}

func TestManagedRefusesLegacyAndIncompatibleProfile(t *testing.T) {
	for _, executor := range []Executor{nil, testExecutor{profile: "managed-test-v1"}} {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "jobs"), 0700); err != nil {
			t.Fatal(err)
		}
		legacy := filepath.Join(root, "jobs", "legacy.json")
		if err := os.WriteFile(legacy, []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenManaged(root, executor); err == nil {
			t.Fatal("adopted unmanaged legacy jobs")
		}
		if _, err := os.Stat(filepath.Join(root, "acceptance", "owner.json")); !os.IsNotExist(err) {
			t.Fatal("refusal wrote owner configuration", err)
		}
		if data, err := os.ReadFile(legacy); err != nil || string(data) != "{}" {
			t.Fatal("legacy data changed", err)
		}
	}
	root := t.TempDir()
	p, err := OpenManaged(root, testExecutor{profile: "managed-test-v1"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "acceptance", "owner.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, executor := range []Executor{nil, testExecutor{profile: "managed-test-v2"}} {
		if _, err := OpenManaged(root, executor); err == nil {
			t.Fatal("accepted incompatible execution profile")
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refusal changed retained configuration", err)
	}
	if _, err := OpenWithExecutor(root, p.LogicalOwner(), testExecutor{profile: "managed-test-v1"}); err != nil {
		t.Fatal(err)
	}
}

func TestManagedCompatibleExplicitOwnerAndExplicitOpenSemantics(t *testing.T) {
	root := t.TempDir()
	p, err := Open(root, "configured-owner")
	if err != nil {
		t.Fatal(err)
	}
	managed, err := OpenManaged(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if managed.LogicalOwner() != p.LogicalOwner() || managed.config.Epoch != p.config.Epoch {
		t.Fatal("compatible explicit identity replaced")
	}
	for _, owner := range []string{"", "different-owner"} {
		if _, err := Open(root, owner); err == nil {
			t.Fatal("explicit Open accepted", owner)
		}
		if _, err := OpenWithExecutor(root, owner, nil); err == nil {
			t.Fatal("explicit OpenWithExecutor accepted", owner)
		}
	}
	if _, err := OpenManaged("", nil); err == nil {
		t.Fatal("managed open accepted empty root")
	}
}
