package acceptanceprovider

import (
	"context"
	"sync"
	"testing"
	"time"

	job "github.com/openabstractions/abstraction-job/go"
)

// idleExecutor serves nothing and returns when its context ends.
type idleExecutor struct{ testExecutor }

func (idleExecutor) Serve(ctx context.Context, _ job.Store) error {
	<-ctx.Done()
	return nil
}

// JOB-A18: a recheck pass the provider's clock started returns before
// Execute does. Execute does not return while the clock's decision is still
// out, and once it returns the store is written no more.
func TestExecuteWaitsForItsClocksRecheckPass(t *testing.T) {
	p, err := OpenWithExecutor(t.TempDir(), "logical-owner", idleExecutor{testExecutor{profile: "idle-v1"}})
	if err != nil {
		t.Fatal(err)
	}
	binding := localBinding(t)
	receipt := accept(t, p.BindBinding(binding), submission(p, "clock"))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	p.SetStanding(func(context.Context, AuthenticatedSubject, string) (string, error) {
		once.Do(func() { close(entered) })
		<-release
		return "rights:denied", nil
	})
	p.standing.interval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		//unchecked: the idle executor returns nil when its context ends
		_ = p.Execute(ctx)
	}()
	<-entered
	cancel()
	select {
	case <-returned:
		t.Fatal("Execute returned while its clock's recheck was still deciding")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-returned
	before, err := p.store.Load(receipt.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	after, err := p.store.Load(receipt.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt.Time) || after.State != before.State {
		t.Fatalf("the store was written after Execute returned: %+v then %+v", before, after)
	}
}
