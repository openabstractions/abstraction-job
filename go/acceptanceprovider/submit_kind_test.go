package acceptanceprovider

import (
	"context"
	"runtime"
	"sync"
	"testing"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

// JOB-A13: the method policy deciding Submit sees the submission's kind, so
// a rule can scope submission to one kind of work; no other method carries a
// kind.
func TestTheSubmitPolicySeesTheSubmissionsKind(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("program proof is refused on this platform before any policy runs")
	}
	p := openTest(t, t.TempDir())
	var mu sync.Mutex
	seen := map[string]string{}
	policy := func(ctx context.Context, _ *identity.Peer, service, method string) error {
		kind, ok := SubmitKind(ctx)
		mu.Lock()
		defer mu.Unlock()
		if ok {
			seen[method] = kind
		} else {
			seen[method] = "<none>"
		}
		return nil
	}
	s := submission(p, "kinded")
	s.Kind = "inference"
	if err := methodExchange(t, p, policy, func(transport listen.FrameClient) {
		if r, err := api.NewRecoverableAcceptanceClient(transport).Submit(s); err != nil || r.Outcome.String() != "accepted" {
			t.Fatalf("submit %+v %v", r, err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := methodExchange(t, p, policy, func(transport listen.FrameClient) {
		if r, err := api.NewOperationControlClient(transport).ObserveWork(s.Identity); err != nil || r.Outcome.String() != "observed" {
			t.Fatalf("observe %+v %v", r, err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if seen["Submit"] != "inference" || seen["ObserveWork"] != "<none>" {
		t.Fatalf("the policy saw %v", seen)
	}
}
