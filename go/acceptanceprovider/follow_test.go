package acceptanceprovider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	"github.com/openabstractions/abstraction-identity/remote"
	job "github.com/openabstractions/abstraction-job/go"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	watch "github.com/openabstractions/abstraction-watch/go"
)

// followPipes numbers this process's follow test pipes.
var followPipes atomic.Int64

func TestFollowWorkVisibleChangeRenewalAndRestart(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	s := submission(p, "follow")
	r := accept(t, p.Bind("alice"), s)
	b := p.BindOperations("alice")
	first, err := b.FollowWork(s.Identity, "", 0)
	if err != nil || first.Outcome != api.WorkObservationOutcomeChanged || first.Snapshot == nil || first.Mechanism != api.WorkObservationMechanismPolled {
		t.Fatal(first, err)
	}
	claim, err := p.store.Claim(r.OperationID, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	running, err := b.FollowWork(s.Identity, first.Next, 0)
	if err != nil || running.Outcome != api.WorkObservationOutcomeChanged || running.Snapshot.State != api.WorkStateRunning {
		t.Fatal(running, err)
	}
	if _, err := p.store.Renew(claim.ID, claim.Lease.Epoch, time.Minute); err != nil {
		t.Fatal(err)
	}
	quiet, err := b.FollowWork(s.Identity, running.Next, 10)
	if err != nil || quiet.Outcome != api.WorkObservationOutcomeUnchanged || quiet.Next != running.Next || quiet.Snapshot != nil {
		t.Fatal(quiet, err)
	}
	if _, err := p.store.Update(claim.ID, claim.Lease.Epoch, func(record *job.Record) error { record.Progress.Done = 1; record.Progress.Total = 2; return nil }); err != nil {
		t.Fatal(err)
	}
	progress, err := b.FollowWork(s.Identity, running.Next, 0)
	if err != nil || progress.Outcome != api.WorkObservationOutcomeChanged || progress.Snapshot.Progress.Done != 1 {
		t.Fatal(progress, err)
	}
	restarted := openTest(t, root)
	gap, err := restarted.BindOperations("alice").FollowWork(s.Identity, progress.Next, 0)
	if err != nil || gap.Outcome != api.WorkObservationOutcomeGap || gap.Snapshot != nil || gap.Next != progress.Next {
		t.Fatal(gap, err)
	}
	fresh, err := restarted.BindOperations("alice").FollowWork(s.Identity, "", 0)
	if err != nil || fresh.Outcome != api.WorkObservationOutcomeChanged || fresh.Next == progress.Next || fresh.Snapshot == nil {
		t.Fatal(fresh, err)
	}
}

func TestFollowWorkScopeAbsenceBoundsAndIdentity(t *testing.T) {
	p := openTest(t, t.TempDir())
	s := submission(p, "absent-follow")
	b := p.BindOperations("alice")
	absent, err := b.FollowWork(s.Identity, "", 30000)
	if err != nil || absent.Outcome != api.WorkObservationOutcomeUnknown || absent.Snapshot != nil {
		t.Fatal(absent, err)
	}
	if _, err := os.Stat(p.requestPath("alice", s.Identity)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("absence sealed", err)
	}
	accept(t, p.Bind("alice"), s)
	first, _ := b.FollowWork(s.Identity, "", 0)
	for _, other := range []api.OperationControl{p.BindOperations("bob"), p.BindOperations("")} {
		page, err := other.FollowWork(s.Identity, first.Next, 0)
		if err != nil || page.Outcome != api.WorkObservationOutcomeForbidden || page.Snapshot != nil || page.Next != "" {
			t.Fatal(page, err)
		}
	}
	other := s.Identity
	other.Key = "other"
	page, err := b.FollowWork(other, first.Next, 0)
	if err != nil || page.Outcome != api.WorkObservationOutcomeForbidden {
		t.Fatal(page, err)
	}
	for _, wait := range []int64{-1, 30001} {
		page, err := b.FollowWork(s.Identity, "", wait)
		if err != nil || page.Outcome != api.WorkObservationOutcomeInvalid {
			t.Fatal(page, err)
		}
	}
	page, err = b.FollowWork(s.Identity, "malformed", 0)
	if err != nil || page.Outcome != api.WorkObservationOutcomeInvalid {
		t.Fatal(page, err)
	}
}

func TestFollowWorkGeneratedBoundsAndUnavailablePolicy(t *testing.T) {
	p := openTest(t, t.TempDir())
	s := submission(p, "bounded-follow")
	before := storageTree(t, p.root)
	var calls atomic.Int32
	transport := frames{p, "alice", func(string, string, string) access { calls.Add(1); return accessUnavailable }}
	client := api.NewOperationControlClient(transport)
	for _, wait := range []int64{-1, 30001} {
		if _, err := client.FollowWork(s.Identity, "", wait); err == nil {
			t.Fatal("generated bounds accepted", wait)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid argument reached policy")
	}
	page, err := client.FollowWork(s.Identity, "", 30000)
	if err != nil || page.Outcome != api.WorkObservationOutcomeUnavailable || page.Snapshot != nil || page.Next != "" {
		t.Fatal(page, err)
	}
	if !reflect.DeepEqual(before, storageTree(t, p.root)) {
		t.Fatal("refused observation changed durable state")
	}
}

func awaitWorkWaiter(t *testing.T, gate *watch.Gate) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for gate.Waiting() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("wait not held")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFollowWorkWakesAndRechecksAuthority(t *testing.T) {
	p := openTest(t, t.TempDir())
	s := submission(p, "wake-follow")
	r := accept(t, p.Bind("alice"), s)
	claim, err := p.store.Claim(r.OperationID, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	b := p.BindOperations("alice").(*bound)
	first, _ := b.FollowWork(s.Identity, "", 0)
	_, gate := p.workObservationSource()
	var allowed atomic.Bool
	allowed.Store(true)
	result := make(chan api.WorkObservation, 1)
	go func() {
		page, _ := b.followWork(t.Context(), s.Identity, first.Next, 3000, func() access {
			if allowed.Load() {
				return accessAllowed
			}
			return accessDenied
		})
		result <- page
	}()
	awaitWorkWaiter(t, gate)
	allowed.Store(false)
	if _, err := p.store.Update(claim.ID, claim.Lease.Epoch, func(record *job.Record) error { record.Progress.Done = 1; return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case page := <-result:
		if page.Outcome != api.WorkObservationOutcomeForbidden || page.Snapshot != nil || page.Next != "" {
			t.Fatal(page)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("job.Watch did not wake the wait")
	}
	if gate.Waiting() != 0 {
		t.Fatal("waiter leaked")
	}
}

func TestFollowWorkCancellationJoinsAndBusyRefuses(t *testing.T) {
	p := openTest(t, t.TempDir())
	s := submission(p, "cancel-follow")
	accept(t, p.Bind("alice"), s)
	b := p.BindOperations("alice").(*bound)
	first, _ := b.FollowWork(s.Identity, "", 0)
	p.observation.gate = watch.NewGate(1)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := b.followWork(ctx, s.Identity, first.Next, 30000, func() access { return accessAllowed })
		done <- err
	}()
	awaitWorkWaiter(t, p.observation.gate)
	busy, err := b.FollowWork(s.Identity, first.Next, 1)
	if err != nil || busy.Outcome != api.WorkObservationOutcomeUnavailable || busy.Snapshot != nil {
		t.Fatal(busy, err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not join")
	}
	if p.observation.gate.Waiting() != 0 {
		t.Fatal("waiter leaked")
	}
}

func TestFollowWorkNativeCancellationAndPostWakePolicy(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("Program proof unavailable")
	}
	for _, action := range []string{"cancel", "withdraw", "unavailable", "policy-cancel"} {
		t.Run(action, func(t *testing.T) {
			p := openTest(t, t.TempDir())
			s := submission(p, "native-follow")
			accept(t, p.Bind("alice"), s)
			first, _ := p.BindOperations("alice").FollowWork(s.Identity, "", 0)
			_, gate := p.workObservationSource()
			endpoint := fmt.Sprintf(`\\.\pipe\oa-follow-test-%d-%d`, os.Getpid(), followPipes.Add(1))
			if runtime.GOOS != "windows" {
				dir, err := os.MkdirTemp("", "oa-follow-")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.RemoveAll(dir); err != nil {
						t.Error(err)
					}
				})
				endpoint = filepath.Join(dir, "follow.sock")
			}
			listener, err := listen.Listen(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			defer cancel()
			var refused atomic.Bool
			entered := make(chan struct{}, 1)
			server := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err == nil {
					err = HandleConnectionWithPolicy(ctx, conn, p, func(*identity.Peer) (string, error) { return "alice", nil }, func(policyCtx context.Context, _ *identity.Peer, _, _ string) error {
						if refused.Load() {
							if action == "policy-cancel" {
								entered <- struct{}{}
								<-policyCtx.Done()
								return policyCtx.Err()
							}
							if action == "unavailable" {
								return ErrPolicyUnavailable
							}
							return errors.New("withdrawn")
						}
						return nil
					})
				}
				server <- err
			}()
			callCtx, stop := context.WithCancel(ctx)
			defer stop()
			client := api.NewOperationControlClient((listen.FrameClient{Endpoint: endpoint, MaxFrame: MaxFrameBytes}).WithContext(callCtx))
			result := make(chan api.WorkObservation, 1)
			clientErr := make(chan error, 1)
			go func() {
				page, err := client.FollowWork(s.Identity, first.Next, 30000)
				result <- page
				clientErr <- err
			}()
			awaitWorkWaiter(t, gate)
			if action == "cancel" {
				stop()
			} else {
				refused.Store(true)
				gate.Advance()
				if action == "policy-cancel" {
					select {
					case <-entered:
						stop()
					case <-ctx.Done():
						t.Fatal("policy did not receive wake")
					}
				}
			}
			select {
			case page := <-result:
				err := <-clientErr
				if action == "cancel" || action == "policy-cancel" {
					if !errors.Is(err, context.Canceled) {
						t.Fatal(page, err)
					}
				} else {
					want := api.WorkObservationOutcomeForbidden
					if action == "unavailable" {
						want = api.WorkObservationOutcomeUnavailable
					}
					if err != nil || page.Outcome != want || page.Snapshot != nil || page.Next != "" {
						t.Fatal(page, err)
					}
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			select {
			case err := <-server:
				if action != "cancel" && action != "policy-cancel" && err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("native waiter did not join")
			}
			if gate.Waiting() != 0 {
				t.Fatal("native waiter leaked")
			}
		})
	}
}

func TestFollowWorkRemoteRechecksExactPinAfterWake(t *testing.T) {
	p := openTest(t, t.TempDir())
	var admitted atomic.Bool
	admitted.Store(true)
	peer := remoteSubjectPeer("follow-pin")
	handler := p.RemoteHandler(RemoteConfig{AccountKind: "posix", Account: "1000", Admit: func(remote.Peer) error {
		if !admitted.Load() {
			return errors.New("pin withdrawn")
		}
		return nil
	}}, func(context.Context, remote.Peer, string, string) error { return nil })
	frames := subjectRemoteFrames{handler: handler, peer: peer}
	s := submission(p, "remote-follow")
	accept(t, api.NewRecoverableAcceptanceClient(frames), s)
	client := api.NewOperationControlClient(frames)
	first, err := client.FollowWork(s.Identity, "", 0)
	if err != nil || first.Snapshot == nil {
		t.Fatal(first, err)
	}
	_, gate := p.workObservationSource()
	done := make(chan api.WorkObservation, 1)
	go func() { page, _ := client.FollowWork(s.Identity, first.Next, 2000); done <- page }()
	awaitWorkWaiter(t, gate)
	admitted.Store(false)
	gate.Advance()
	select {
	case page := <-done:
		if page.Outcome != api.WorkObservationOutcomeForbidden || page.Snapshot != nil || page.Next != "" {
			t.Fatal(page)
		}
	case <-time.After(time.Second):
		t.Fatal("remote authority wait did not join")
	}
}

func TestFollowWorkUsesCanonicalAcceptanceScope(t *testing.T) {
	p := openTest(t, t.TempDir())
	lease, err := AcquireHost(p.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	})
	enableCanonical(t, p)
	a, b := canonicalVersions(t)
	s := submission(p, "canonical-follow")
	accept(t, p.BindBinding(a), s)
	first, err := p.BindOperationsBinding(a).FollowWork(s.Identity, "", 0)
	if err != nil || first.Snapshot == nil {
		t.Fatal(first, err)
	}
	continued, err := p.BindOperationsBinding(b).FollowWork(s.Identity, first.Next, 0)
	if err != nil || continued.Outcome != api.WorkObservationOutcomeUnchanged || continued.Next != first.Next {
		t.Fatal(continued, err)
	}
	other := *b.Subject
	other.Program = "msix:OtherFixture_pzs8sxrjxfjjc"
	b.Subject = &other
	refused, err := p.BindOperationsBinding(b).FollowWork(s.Identity, first.Next, 0)
	if err != nil || refused.Outcome != api.WorkObservationOutcomeForbidden || refused.Snapshot != nil {
		t.Fatal(refused, err)
	}
}
