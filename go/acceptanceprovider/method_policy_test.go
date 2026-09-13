package acceptanceprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func methodExchange(t *testing.T, p *Provider, policy MethodPolicy, invoke func(listen.FrameClient), cancelAfterPolicy ...bool) error {
	t.Helper()
	endpoint := fmt.Sprintf(`\\.\pipe\oa-policy-%d`, time.Now().UnixNano())
	if runtime.GOOS != "windows" {
		dir, err := os.MkdirTemp("", "oa-policy-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		endpoint = filepath.Join(dir, "p.sock")
	}
	l, err := listen.Listen(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if len(cancelAfterPolicy) > 0 && cancelAfterPolicy[0] {
		original := policy
		policy = func(ctx context.Context, peer *identity.Peer, service, method string) error {
			err := original(ctx, peer, service, method)
			cancel()
			return err
		}
	}
	done := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if err == nil {
			err = HandleConnectionWithPolicy(ctx, conn, p, func(*identity.Peer) (string, error) { return "caller", nil }, policy)
		}
		done <- err
	}()
	invoke(listen.FrameClient{Endpoint: endpoint, Timeout: time.Second, MaxFrame: MaxFrameBytes})
	select {
	case err := <-done:
		return err
	case <-time.After(4 * time.Second):
		t.Fatal("connection did not finish")
		return context.DeadlineExceeded
	}
}
func TestMethodPolicyAllowsObservationAndRefusesEffects(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	existing := submission(p, "existing")
	accept(t, p.Bind("caller"), existing)
	before := storageTree(t, root)
	var calls atomic.Int32
	policy := func(ctx context.Context, peer *identity.Peer, service, method string) error {
		calls.Add(1)
		if ctx.Err() != nil || peer == nil {
			return errors.New("missing call evidence")
		}
		if _, err := peer.Path.AtLeast(listen.Program.Path); err != nil {
			return err
		}
		if service == "abstraction.job/operations@1" && (method == "ObserveWork" || method == "ReadResult") {
			return nil
		}
		return errors.New("read-only policy")
	}
	var result api.AcceptanceResult
	var clientErr error
	serverErr := methodExchange(t, p, policy, func(transport listen.FrameClient) {
		result, clientErr = api.NewRecoverableAcceptanceClient(transport).Submit(submission(p, "blocked"))
	})
	if runtime.GOOS == "darwin" {
		if !errors.Is(serverErr, identity.ErrNotProven) || clientErr == nil || calls.Load() != 0 {
			t.Fatalf("proof refusal: %v %v %d", serverErr, clientErr, calls.Load())
		}
		return
	}
	if serverErr != nil || clientErr != nil || result.Outcome != "forbidden" {
		t.Fatalf("submit %+v %v %v", result, clientErr, serverErr)
	}
	serverErr = methodExchange(t, p, policy, func(transport listen.FrameClient) {
		r, err := api.NewRecoverableAcceptanceClient(transport).CancelWork(existing.Identity)
		if err != nil || r.Outcome != "forbidden" {
			t.Fatalf("cancel %+v %v", r, err)
		}
	})
	if serverErr != nil {
		t.Fatal(serverErr)
	}
	serverErr = methodExchange(t, p, policy, func(transport listen.FrameClient) {
		r, err := api.NewOperationControlClient(transport).ObserveWork(existing.Identity)
		if err != nil || r.Outcome != "observed" || r.Snapshot.CancellationRequested {
			t.Fatalf("observe %+v %v", r, err)
		}
	})
	if serverErr != nil {
		t.Fatal(serverErr)
	}
	serverErr = methodExchange(t, p, policy, func(transport listen.FrameClient) {
		r, err := api.NewOperationControlClient(transport).ReadResult(existing.Identity, 0, 64)
		if err != nil || r.Outcome != "unsupported" {
			t.Fatalf("read %+v %v", r, err)
		}
	})
	if serverErr != nil {
		t.Fatal(serverErr)
	}
	if calls.Load() != 4 || !reflect.DeepEqual(before, storageTree(t, root)) {
		t.Fatal("method refusal/observation mutated durable state", calls.Load())
	}
}
func TestMalformedMethodNeverReachesPolicy(t *testing.T) {
	p := openTest(t, t.TempDir())
	var calls atomic.Int32
	policy := func(context.Context, *identity.Peer, string, string) error { calls.Add(1); return nil }
	for _, frame := range []string{
		`{"version":1,"service":"abstraction.job/acceptance@1","method":"Missing","arguments":{}}`,
		`{"version":1,"service":"abstraction.job/acceptance@1","method":"Submit","arguments":{}}`,
		`{"version":1,"service":"unrecognized","method":"GetHistoryWindow","arguments":{}}`,
	} {
		methodExchange(t, p, policy, func(transport listen.FrameClient) {
			reply, err := transport.ExchangeFrame([]byte(frame))
			if err == nil {
				var response struct {
					Ok      bool `json:"ok"`
					Payload struct {
						Code string `json:"code"`
					} `json:"payload"`
				}
				if json.Unmarshal(reply, &response) != nil || response.Ok || response.Payload.Code == "" {
					t.Fatalf("missing dispatch refusal: %s", reply)
				}
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("invalid envelope/arguments reached policy", calls.Load())
	}
}

func TestMethodPolicyCancellationPreventsEffects(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	before := storageTree(t, root)
	var calls atomic.Int32
	policy := func(context.Context, *identity.Peer, string, string) error { calls.Add(1); return nil }
	methodExchange(t, p, policy, func(transport listen.FrameClient) {
		result, err := api.NewRecoverableAcceptanceClient(transport).Submit(submission(p, "cancelled-policy"))
		if err == nil && result.Outcome != "forbidden" {
			t.Fatalf("cancelled policy admitted %+v", result)
		}
	}, true)
	if runtime.GOOS != "darwin" && calls.Load() != 1 {
		t.Fatal("policy not exercised", calls.Load())
	}
	if !reflect.DeepEqual(before, storageTree(t, root)) {
		t.Fatal("cancelled policy mutated durable state")
	}
}
