package acceptanceprovider

import (
	"context"
	"errors"
	"testing"
	"time"

	job "github.com/openabstractions/abstraction-job/go"
)

// JOB-A19 with JOB-A3: a rerun interrupted between its journal write and
// its new operation's insertion is completed by recovery. The reopened
// provider holds the new operation, unclaimed, and a duplicate submission
// returns it; the refused operation stays failed.
func TestARerunInterruptedBeforeItsOperationIsRecovered(t *testing.T) {
	for _, point := range []string{"after-journal", "after-job", "before-reply"} {
		t.Run(point, func(t *testing.T) {
			root := t.TempDir()
			executor := testExecutor{profile: "test-v1", prefix: "private/"}
			p, err := OpenWithExecutor(root, "logical-owner", executor)
			if err != nil {
				t.Fatal(err)
			}
			rules := &standingRules{}
			p.SetStanding(rules.decide)
			binding := localBinding(t)
			s := submission(p, "interrupted-rerun")
			first := accept(t, p.BindBinding(binding), s)
			// A recheck fails the refused operation, as a Block would.
			rules.set("rights:denied", false)
			p.Recheck(context.Background())
			if record, err := p.store.Load(first.OperationID); err != nil || record.State != job.StateFailed {
				t.Fatalf("refused operation %+v %v", record, err)
			}

			rules.set("", false)
			fired := false
			p.fault = func(at string) error {
				if at == point {
					fired = true
					return errors.New("interrupted")
				}
				return nil
			}
			if result, err := p.BindBinding(binding).Submit(s); err != nil || result.Outcome.String() != "unknown" {
				t.Fatalf("interrupted rerun answered %+v %v", result, err)
			}
			if !fired {
				t.Fatal("the rerun did not reach the boundary")
			}

			p, err = OpenWithExecutor(root, "logical-owner", executor)
			if err != nil {
				t.Fatal(err)
			}
			p.SetStanding(rules.decide)
			again := accept(t, p.BindBinding(binding), s)
			if again.OperationID == first.OperationID {
				t.Fatal("recovery answered the refused operation")
			}
			record, err := p.store.Load(again.OperationID)
			if err != nil || record.State.Terminal() || record.Lease.Held(time.Now()) {
				t.Fatalf("recovered rerun %+v %v", record, err)
			}
			if old, err := p.store.Load(first.OperationID); err != nil || old.State != job.StateFailed {
				t.Fatalf("the refused operation changed: %+v %v", old, err)
			}
			if duplicate := accept(t, p.BindBinding(binding), s); duplicate.OperationID != again.OperationID {
				t.Fatalf("a duplicate after recovery named %s, want %s", duplicate.OperationID, again.OperationID)
			}
		})
	}
}
