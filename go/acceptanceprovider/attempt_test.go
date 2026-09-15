package acceptanceprovider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	job "github.com/openabstractions/abstraction-job/go"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

// endOperation moves an accepted operation to a terminal state, as its executor would.
func endOperation(t *testing.T, p *Provider, operationID string, state job.State) {
	t.Helper()
	claimed, err := p.store.Claim(operationID, "test-executor", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.store.Update(operationID, claimed.Lease.Epoch, func(r *job.Record) error {
		r.State = state
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func retry(s api.Submission, attempt int64) api.Submission {
	s.Identity.Attempt = attempt
	return s
}

func outcomeOf(t *testing.T) func(api.AcceptanceResult, error) string {
	return func(v api.AcceptanceResult, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if v.Outcome != "accepted" && v.Receipt != nil {
			t.Fatalf("receipt on %s", v.Outcome)
		}
		return v.Outcome
	}
}

func TestRetryAttemptRequiresTerminalFailure(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	c := p.Bind("alice")
	original := submission(p, "retry-key")
	first := accept(t, c, original)

	// A live original makes every retry ineligible without sealing it.
	if got := outcomeOf(t)(c.Submit(retry(original, 1))); got != "invalid" {
		t.Fatalf("retry of live work: %s", got)
	}
	if got := outcomeOf(t)(c.Reconcile(retry(original, 1).Identity)); got != "invalid" {
		t.Fatalf("reconcile of ineligible attempt: %s", got)
	}
	if got := outcomeOf(t)(c.Submit(retry(original, 2))); got != "invalid" {
		t.Fatalf("skipped attempt: %s", got)
	}
	if got := outcomeOf(t)(c.Submit(retry(original, -1))); got != "invalid" {
		t.Fatalf("negative attempt: %s", got)
	}
	endOperation(t, p, first.OperationId, job.StateComplete)
	if got := outcomeOf(t)(c.Submit(retry(original, 1))); got != "invalid" {
		t.Fatalf("retry of completed work: %s", got)
	}
	if jobs(t, root) != 1 {
		t.Fatalf("ineligible attempts created work: %d", jobs(t, root))
	}
}

func TestRetryAttemptAfterTerminalFailure(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	c := p.Bind("alice")
	original := submission(p, "failed-key")
	first := accept(t, c, original)
	endOperation(t, p, first.OperationId, job.StateFailed)

	changed := retry(original, 1)
	changed.Spec = []byte(`{"source":"other"}`)
	if got := outcomeOf(t)(c.Submit(changed)); got != "key_conflict" {
		t.Fatalf("retry with different arguments: %s", got)
	}
	if got := outcomeOf(t)(c.Submit(retry(original, 1))); got != "accepted" {
		t.Fatalf("retry after failure: %s", got)
	}

	// Concurrent duplicates of one attempt converge on one operation.
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := p.Bind("alice").Submit(retry(original, 1))
			if err == nil && v.Receipt != nil {
				ids <- v.Receipt.OperationId
			}
		}()
	}
	wg.Wait()
	close(ids)
	var second string
	for id := range ids {
		if second == "" {
			second = id
		} else if id != second {
			t.Fatalf("duplicate attempt created %s and %s", second, id)
		}
	}
	if second == "" || second == first.OperationId {
		t.Fatalf("retry operation %q reused the failed operation", second)
	}
	if jobs(t, root) != 2 {
		t.Fatalf("operations: %d", jobs(t, root))
	}

	// Each attempt keeps its own receipt, including across provider restart.
	p = openTest(t, root)
	c = p.Bind("alice")
	if v, err := c.Reconcile(original.Identity); err != nil || v.Receipt == nil || v.Receipt.OperationId != first.OperationId {
		t.Fatalf("original attempt after restart: %+v %v", v, err)
	}
	if v, err := c.Submit(retry(original, 1)); err != nil || v.Receipt == nil || v.Receipt.OperationId != second || v.Receipt.Identity.Attempt != 1 {
		t.Fatalf("retry after restart: %+v %v", v, err)
	}
	if v, err := p.Bind("bob").Reconcile(retry(original, 1).Identity); err != nil || v.Receipt != nil {
		t.Fatalf("other caller saw retry: %+v %v", v, err)
	}

	// A cancelled retry makes the next attempt ineligible.
	endOperation(t, p, second, job.StateCancelled)
	if got := outcomeOf(t)(c.Submit(retry(original, 2))); got != "invalid" {
		t.Fatalf("retry after cancellation: %s", got)
	}
}

func TestRetryAttemptAfterSealedAttempt(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	c := p.Bind("alice")
	original := submission(p, "sealed-key")
	if got := outcomeOf(t)(c.Reconcile(original.Identity)); got != "definitely_not_accepted" {
		t.Fatalf("seal original: %s", got)
	}
	if got := outcomeOf(t)(c.Submit(original)); got != "definitely_not_accepted" {
		t.Fatalf("sealed original accepted: %s", got)
	}
	if got := outcomeOf(t)(c.Submit(retry(original, 1))); got != "accepted" {
		t.Fatalf("retry after seal: %s", got)
	}
	if jobs(t, root) != 1 {
		t.Fatalf("operations: %d", jobs(t, root))
	}
}

func TestOriginalAttemptKeepsJournalPath(t *testing.T) {
	p := openTest(t, t.TempDir())
	id := api.RequestIdentity{Key: "k", HistoryEpoch: p.config.Epoch}
	b, _ := json.Marshal([]string{"alice", "abstraction.job/acceptance@1", id.HistoryEpoch, id.Key})
	h := sha256.Sum256(b)
	if got := filepath.Base(p.requestPath("alice", id)); got != hex.EncodeToString(h[:])+".json" {
		t.Fatalf("attempt zero moved journals: %s", got)
	}
	id.Attempt = 1
	if filepath.Base(p.requestPath("alice", id)) == hex.EncodeToString(h[:])+".json" {
		t.Fatal("retry shares the original journal")
	}
}
