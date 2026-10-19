package acceptanceprovider

import (
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/openabstractions/abstraction-identity/remote"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

// No transport, listener or executor is launched. A valid receiver mapping alone
// must not dispatch any job method to its authorized provider binding.
func TestRemoteHandlerWithoutPolicyRefusesEveryMethod(t *testing.T) {
	e := newCheckingExecutor()
	p, err := OpenWithExecutor(t.TempDir(), "owner", e)
	if err != nil {
		t.Fatal(err)
	}
	s := submission(p, "nil-policy")
	before, err := os.ReadDir(p.requests())
	if err != nil {
		t.Fatal(err)
	}
	{
		peer := remoteSubjectPeer("nil-policy")
		handler := p.RemoteHandler(RemoteConfig{AccountKind: "posix", Account: "1000", Admit: func(remote.Peer) error { return nil }}, nil)
		frames := subjectRemoteFrames{handler: handler, peer: peer}
		a := api.NewRecoverableAcceptanceClient(frames)
		o := api.NewOperationControlClient(frames)
		i := api.NewJobInventoryClient(frames)
		operator := api.NewJobOperatorClient(frames)
		if _, err := a.GetHistoryWindow(); err == nil {
			t.Fatal("history disclosed without policy")
		} else {
			var service *api.ServiceError
			if !errors.As(err, &service) || service.Code != api.ServiceErrorCodeForbidden {
				t.Fatalf("history: %v", err)
			}
		}
		check := func(method, outcome string, err error) {
			t.Helper()
			if err != nil || outcome != "forbidden" {
				t.Fatalf("%s: %s %v", method, outcome, err)
			}
		}
		v, err := a.Submit(s)
		check("Submit", v.Outcome.String(), err)
		v, err = a.Reconcile(s.Identity)
		check("Reconcile", v.Outcome.String(), err)
		cancel, err := a.CancelWork(s.Identity)
		check("CancelWork", cancel.Outcome.String(), err)
		observe, err := o.ObserveWork(s.Identity)
		check("ObserveWork", observe.Outcome.String(), err)
		read, err := o.ReadResult(s.Identity, 0, 1)
		check("ReadResult", read.Outcome.String(), err)
		link, err := o.LinkResult(s.Identity, "unused")
		check("LinkResult", link.Outcome.String(), err)
		page, err := i.ListWork("", 1)
		check("ListWork", page.Outcome.String(), err)
		page, err = operator.ListAccountWork("", 1)
		check("ListAccountWork", page.Outcome.String(), err)
		operation, err := operator.CancelOperation("unused")
		check("CancelOperation", operation.Outcome.String(), err)
	}
	after, err := os.ReadDir(p.requests())
	if err != nil || !reflect.DeepEqual(before, after) || *e.asked != 0 {
		t.Fatalf("nil policy reached admission/state: before=%v after=%v asked=%d err=%v", before, after, *e.asked, err)
	}
}
