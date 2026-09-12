package acceptanceprovider

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	job "github.com/openabstractions/abstraction-job/go"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

func openTest(t *testing.T, root string) *Provider {
	t.Helper()
	p, err := Open(root, "logical-owner")
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func submission(p *Provider, key string) api.Submission {
	return api.Submission{Identity: api.RequestIdentity{Key: key, HistoryEpoch: p.config.Epoch}, Kind: "download", Spec: []byte(`{"source":"test"}`), RequiredGuarantees: []string{GuaranteeServiceRestart}}
}
func accept(t *testing.T, c api.RecoverableAcceptance, s api.Submission) api.Receipt {
	t.Helper()
	v, err := c.Submit(s)
	if err != nil || v.Outcome != "accepted" || v.Receipt == nil {
		t.Fatalf("submit: %+v %v", v, err)
	}
	return *v.Receipt
}
func jobs(t *testing.T, root string) int {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "jobs", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return len(paths)
}

func TestRestartDuplicateAndScopedIdentity(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	s := submission(p, "stable-key")
	r := accept(t, p.Bind("alice"), s)
	p = openTest(t, root)
	v, err := p.Bind("alice").Reconcile(s.Identity)
	if err != nil || v.Receipt == nil || v.Receipt.OperationId != r.OperationId {
		t.Fatalf("restart: %+v %v", v, err)
	}
	if duplicate := accept(t, p.Bind("alice"), s); duplicate.OperationId != r.OperationId {
		t.Fatal("duplicate ID")
	}
	changed := s
	changed.Spec = []byte(`{"source":"different"}`)
	v, _ = p.Bind("alice").Submit(changed)
	if v.Outcome != "key_conflict" {
		t.Fatalf("different arguments: %+v", v)
	}
	other := accept(t, p.Bind("bob"), s)
	if other.OperationId == r.OperationId || jobs(t, root) != 2 {
		t.Fatal("caller scopes aliased")
	}
	v, _ = p.Bind("").Reconcile(s.Identity)
	if v.Outcome != "forbidden" {
		t.Fatal(v)
	}
	if _, err = p.Bind("").GetHistoryWindow(); err == nil {
		t.Fatal("anonymous window")
	}
}

func TestNegativeSealsDelayedSubmission(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	s := submission(p, "delayed")
	v, _ := p.Bind("alice").Reconcile(s.Identity)
	if v.Outcome != "definitely_not_accepted" {
		t.Fatal(v)
	}
	p = openTest(t, root)
	v, _ = p.Bind("alice").Submit(s)
	if v.Outcome != "definitely_not_accepted" || jobs(t, root) != 0 {
		t.Fatalf("delayed submit: %+v", v)
	}
}

func TestNegativeVersusSubmitRace(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	q := openTest(t, root)
	for i := 0; i < 20; i++ {
		s := submission(p, job.NewID())
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		var submitted, reconciled api.AcceptanceResult
		go func() { defer wg.Done(); <-start; submitted, _ = p.Bind("alice").Submit(s) }()
		go func() { defer wg.Done(); <-start; reconciled, _ = q.Bind("alice").Reconcile(s.Identity) }()
		close(start)
		wg.Wait()
		if submitted.Outcome != reconciled.Outcome {
			t.Fatalf("race outcomes disagree: %+v %+v", submitted, reconciled)
		}
		if submitted.Outcome != "accepted" && submitted.Outcome != "definitely_not_accepted" {
			t.Fatal(submitted)
		}
		if submitted.Outcome == "accepted" && submitted.Receipt.OperationId != reconciled.Receipt.OperationId {
			t.Fatal("race duplicated operation")
		}
	}
}

func TestConcurrentDuplicateSubmissions(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	q := openTest(t, root)
	s := submission(p, "concurrent")
	results := make(chan api.AcceptanceResult, 8)
	for i := 0; i < 8; i++ {
		go func(i int) {
			provider := p
			if i%2 == 1 {
				provider = q
			}
			v, _ := provider.Bind("alice").Submit(s)
			results <- v
		}(i)
	}
	operation := ""
	for i := 0; i < 8; i++ {
		v := <-results
		if v.Outcome != "accepted" || v.Receipt == nil {
			t.Fatal(v)
		}
		if operation == "" {
			operation = v.Receipt.OperationId
		}
		if operation != v.Receipt.OperationId {
			t.Fatal("different concurrent operation")
		}
	}
	if jobs(t, root) != 1 {
		t.Fatal("multiple concurrent jobs")
	}
}

func TestCrashChild(t *testing.T) {
	root := os.Getenv("OA_ACCEPTANCE_CRASH_ROOT")
	if root == "" {
		return
	}
	p, err := Open(root, "logical-owner")
	if err != nil {
		t.Fatal(err)
	}
	p.fault = func(point string) error {
		if point == "after-job" && os.Getenv("OA_ACCEPTANCE_CRASH_POINT") == "after-job-complete" {
			s := submission(p, "crash-key")
			data, err := os.ReadFile(p.requestPath("alice", s.Identity))
			if err != nil {
				t.Fatal(err)
			}
			j, err := p.decodeJournal(data)
			if err != nil {
				t.Fatal(err)
			}
			r, err := p.store.Claim(j.Receipt.OperationId, "test-worker", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.store.Update(r.ID, r.Lease.Epoch, func(r *job.Record) error { r.State = job.StateComplete; return nil })
			if err != nil {
				t.Fatal(err)
			}
			os.Exit(73)
		}
		if point == os.Getenv("OA_ACCEPTANCE_CRASH_POINT") {
			os.Exit(73)
		}
		return nil
	}
	_, _ = p.Bind("alice").Submit(submission(p, "crash-key"))
	t.Fatal("crash boundary not reached")
}

func TestProcessCrashRecoveryBoundaries(t *testing.T) {
	for _, point := range []string{"after-journal", "after-job", "before-reply", "after-job-complete"} {
		t.Run(point, func(t *testing.T) {
			root := t.TempDir()
			p := openTest(t, root)
			s := submission(p, "crash-key")
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(exe, "-test.run=^TestCrashChild$")
			cmd.Env = append(os.Environ(), "OA_ACCEPTANCE_CRASH_ROOT="+root, "OA_ACCEPTANCE_CRASH_POINT="+point)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 73 {
				t.Fatalf("crash child: %v %s", err, output)
			}
			// The journal already owns the stable operation identity before any job insert.
			data, err := os.ReadFile(p.requestPath("alice", s.Identity))
			if err != nil {
				t.Fatal(err)
			}
			j, err := p.decodeJournal(data)
			if err != nil {
				t.Fatal(err)
			}
			p = openTest(t, root)
			v, _ := p.Bind("alice").Reconcile(s.Identity)
			if v.Outcome != "accepted" || v.Receipt.OperationId != j.Receipt.OperationId || jobs(t, root) != 1 {
				t.Fatalf("recovery: %+v", v)
			}
			if again := accept(t, p.Bind("alice"), s); again.OperationId != j.Receipt.OperationId || jobs(t, root) != 1 {
				t.Fatal("restart duplicate")
			}
			if point == "after-job-complete" {
				r, err := p.store.Load(j.Receipt.OperationId)
				if err != nil || r.State != job.StateComplete {
					t.Fatalf("completed record changed: %+v %v", r, err)
				}
				cancel, _ := p.Bind("alice").CancelWork(s.Identity)
				if cancel.Outcome != "already_terminal" {
					t.Fatal(cancel)
				}
			}
		})
	}
}

func TestAdmissionLimits(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	for _, which := range []string{"key", "spec", "scope", "invalid-json"} {
		s := submission(p, which)
		scope := "alice"
		want := "invalid"
		switch which {
		case "key":
			s.Identity.Key = strings.Repeat("k", 1025)
		case "spec":
			s.Spec = make([]byte, MaxSpecBytes+1)
		case "scope":
			scope = strings.Repeat("c", MaxCallerScopeBytes+1)
			want = "forbidden"
		case "invalid-json":
			s.Spec = []byte(`{"broken":`)
		}
		v, _ := p.Bind(scope).Submit(s)
		if v.Outcome != want {
			t.Fatalf("%s: %+v", which, v)
		}
	}
	if jobs(t, root) != 0 {
		t.Fatal("invalid work admitted")
	}
}

func TestSymlinkEvidence(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	// Platforms without symlink authority report a narrow skip; normal tests remain.
	s := submission(p, "link")
	target := filepath.Join(root, "symlink-target.json")
	if err := os.WriteFile(target, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p.requestPath("alice", s.Identity)); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	v, _ := p.Bind("alice").Reconcile(s.Identity)
	if v.Outcome != "unknown" {
		t.Fatal(v)
	}
	if _, err := Open(root, "logical-owner"); err == nil {
		t.Fatal("symlink recovery opened")
	}
}

type droppedReply struct {
	dispatcher *api.RecoverableAcceptanceDispatcher
}

func (d droppedReply) ExchangeFrame(frame []byte) ([]byte, error) {
	if _, err := d.dispatcher.ExchangeFrame(frame); err != nil {
		return nil, err
	}
	return nil, errors.New("reply lost after durable admission")
}

func TestGeneratedClientLostReplyThenServiceRestart(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	s := submission(p, "wire-key")
	client := api.NewRecoverableAcceptanceClient(droppedReply{&api.RecoverableAcceptanceDispatcher{Handler: p.Bind("alice")}})
	if _, err := client.Submit(s); err == nil {
		t.Fatal("expected lost reply")
	}
	p = openTest(t, root)
	client = api.NewRecoverableAcceptanceClient(&api.RecoverableAcceptanceDispatcher{Handler: p.Bind("alice")})
	v, err := client.Reconcile(s.Identity)
	if err != nil || v.Outcome != "accepted" || jobs(t, root) != 1 {
		t.Fatalf("recovery: %+v %v", v, err)
	}
	r, err := p.store.Load(v.Receipt.OperationId)
	if err != nil || r.Intent != nil {
		t.Fatalf("wait failure cancelled work: %+v %v", r, err)
	}
	duplicate, err := client.Submit(s)
	if err != nil || duplicate.Receipt == nil || duplicate.Receipt.OperationId != v.Receipt.OperationId || jobs(t, root) != 1 {
		t.Fatalf("wire duplicate: %+v %v", duplicate, err)
	}
}

func TestCorruptEvidenceAndMissingPublishedJobNeverCreateReplacement(t *testing.T) {
	for _, which := range []string{"journal", "job", "operation-path"} {
		t.Run(which, func(t *testing.T) {
			root := t.TempDir()
			p := openTest(t, root)
			s := submission(p, "key")
			r := accept(t, p.Bind("alice"), s)
			switch which {
			case "journal":
				if err := os.WriteFile(p.requestPath("alice", s.Identity), []byte(`{`), 0600); err != nil {
					t.Fatal(err)
				}
			case "job":
				if err := os.Remove(filepath.Join(root, "jobs", r.OperationId+".json")); err != nil {
					t.Fatal(err)
				}
			case "operation-path":
				path := p.requestPath("alice", s.Identity)
				data, _ := os.ReadFile(path)
				var j journal
				if err := json.Unmarshal(data, &j); err != nil {
					t.Fatal(err)
				}
				j.Receipt.OperationId = "../outside"
				data, _ = json.Marshal(j)
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			v, _ := p.Bind("alice").Reconcile(s.Identity)
			if v.Outcome != "unknown" {
				t.Fatal(v)
			}
			if _, err := Open(root, "logical-owner"); err == nil {
				t.Fatal("corrupt recovery opened")
			}
			if which == "job" && jobs(t, root) != 0 {
				t.Fatal("missing published job recreated")
			}
		})
	}
}

func TestGuaranteeRefusalOldEpochAndCancellation(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	s := submission(p, "unsupported")
	s.RequiredGuarantees = []string{"power-loss@1"}
	v, _ := p.Bind("alice").Submit(s)
	if v.Outcome != "definitely_not_accepted" {
		t.Fatal(v)
	}
	s.RequiredGuarantees = nil
	v, _ = p.Bind("alice").Submit(s)
	if v.Outcome != "definitely_not_accepted" {
		t.Fatal("seal reopened")
	}
	s = submission(p, "old")
	s.Identity.HistoryEpoch = "forgotten-epoch"
	v, _ = p.Bind("alice").Submit(s)
	if v.Outcome != "unknown" || jobs(t, root) != 0 {
		t.Fatal(v)
	}
	s = submission(p, "cancel")
	r := accept(t, p.Bind("alice"), s)
	c, err := p.Bind("alice").CancelWork(s.Identity)
	if err != nil || c.Outcome != "requested" {
		t.Fatalf("cancel: %+v %v", c, err)
	}
	stored, err := p.store.Load(r.OperationId)
	if err != nil || stored.Intent == nil || stored.Intent.Want != job.WantCancel {
		t.Fatalf("intent: %+v %v", stored, err)
	}
	if stored.State.Terminal() {
		t.Fatal("request falsely marked work stopped")
	}
}
