package acceptanceprovider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

type dropConnectionReply struct{ listen.Conn }

func (d dropConnectionReply) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (d dropConnectionReply) SetDeadline(at time.Time) error {
	return d.Conn.(interface{ SetDeadline(time.Time) error }).SetDeadline(at)
}

// The listener here is test-owned; the production helper only handles one conn.
func localExchange(t *testing.T, p *Provider, auth Authorizer, drop bool, invoke func(*api.RecoverableAcceptanceClient)) error {
	t.Helper()
	endpoint := fmt.Sprintf(`\\.\pipe\oa-acceptance-test-%d`, time.Now().UnixNano())
	if runtime.GOOS != "windows" {
		dir, err := os.MkdirTemp("", "oa-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.RemoveAll(dir); err != nil {
				t.Error(err)
			}
		})
		endpoint = filepath.Join(dir, "accept.sock")
	}
	l, err := listen.Listen(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if err == nil {
			if drop {
				conn = dropConnectionReply{conn}
			}
			err = HandleConnection(ctx, conn, p, auth)
		}
		done <- err
	}()
	client := api.NewRecoverableAcceptanceClient(listen.FrameClient{Endpoint: endpoint, Timeout: 3 * time.Second, MaxFrame: MaxFrameBytes})
	invoke(client)
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return ctx.Err()
	}
}

func TestIdentityBoundConnectionLostReplyAndRestart(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	observed := make(chan string, 3)
	auth := func(peer *identity.Peer) (string, error) {
		user, err := peer.User.AtLeast(listen.Program.User)
		if err != nil {
			return "", err
		}
		scope := fmt.Sprintf("user:%s:%d", user.SID, user.UID)
		observed <- scope
		return scope, nil
	}
	var window api.HistoryWindow
	var clientErr error
	serverErr := localExchange(t, p, auth, false, func(client *api.RecoverableAcceptanceClient) { window, clientErr = client.GetHistoryWindow() })
	if runtime.GOOS == "darwin" {
		if clientErr == nil || !errors.Is(serverErr, identity.ErrNotProven) {
			t.Fatalf("expected current Program proof refusal: %v %v", clientErr, serverErr)
		}
		if len(observed) != 0 || jobs(t, root) != 0 {
			t.Fatal("unproven caller reached provider")
		}
		return
	}
	if serverErr != nil || clientErr != nil {
		t.Fatalf("history: %v %v", serverErr, clientErr)
	}
	s := submission(p, "network-key")
	s.Identity.HistoryEpoch = window.HistoryEpoch
	serverErr = localExchange(t, p, auth, true, func(client *api.RecoverableAcceptanceClient) { _, clientErr = client.Submit(s) })
	if clientErr == nil || serverErr == nil || jobs(t, root) != 1 {
		t.Fatalf("lost reply: %v %v jobs=%d", clientErr, serverErr, jobs(t, root))
	}
	p = openTest(t, root)
	var result api.AcceptanceResult
	serverErr = localExchange(t, p, auth, false, func(client *api.RecoverableAcceptanceClient) { result, clientErr = client.Reconcile(s.Identity) })
	if serverErr != nil || clientErr != nil || result.Outcome != "accepted" || jobs(t, root) != 1 {
		t.Fatalf("reconnect: %+v %v %v", result, clientErr, serverErr)
	}
	scope := <-observed
	if scope != <-observed || scope != <-observed {
		t.Fatal("authenticated scope changed across reconnect")
	}
	stored, _ := p.Bind(scope).Reconcile(s.Identity)
	if stored.Receipt == nil || stored.Receipt.OperationId != result.Receipt.OperationId {
		t.Fatal("wire operation not bound to observed caller")
	}
}

func TestDeniedConnectionDoesNotMutate(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	s := submission(p, "denied")
	for _, auth := range []Authorizer{nil, func(*identity.Peer) (string, error) { return "must-not-use", errors.New("denied") }} {
		var result api.AcceptanceResult
		var clientErr error
		serverErr := localExchange(t, p, auth, false, func(client *api.RecoverableAcceptanceClient) { result, clientErr = client.Submit(s) })
		if runtime.GOOS == "darwin" {
			if clientErr == nil || !errors.Is(serverErr, identity.ErrNotProven) {
				t.Fatalf("proof refusal: %v %v", clientErr, serverErr)
			}
		} else if serverErr != nil || clientErr != nil || result.Outcome != "forbidden" {
			t.Fatalf("deny: %+v %v %v", result, clientErr, serverErr)
		}
	}
	if runtime.GOOS != "darwin" {
		var clientErr error
		serverErr := localExchange(t, p, nil, false, func(client *api.RecoverableAcceptanceClient) { _, clientErr = client.GetHistoryWindow() })
		var refusal *api.ServiceError
		if serverErr != nil || !errors.As(clientErr, &refusal) || refusal.Code != "forbidden" {
			t.Fatalf("denied history window: %v %v", clientErr, serverErr)
		}
	}
	entries, err := os.ReadDir(p.requests())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 || jobs(t, root) != 0 {
		t.Fatal("denied call mutated journal or jobs")
	}
}
