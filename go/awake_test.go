package job

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A machine with no idle-sleep inhibitor cannot prove anything about holds, and
// a bare t.Skip would hide that behind the same silence as a passing test.
func needsAnInhibitor(t *testing.T) {
	t.Helper()
	if err := CanKeepAwake(); err != nil {
		t.Skipf("UNPROVEN  idle-sleep inhibitor — %v", err)
	}
}

func claimed(t *testing.T, s Store, ttl time.Duration) *Record {
	t.Helper()
	id, err := s.Submit(Record{Kind: "test", Spec: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Claim(id, "holder", ttl)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A hold that took nothing must say which of the two reasons it was, because a
// caller answers them differently: a dead lease is the job being over, and a
// machine that cannot be held is a 40 GB transfer about to sleep.
func TestKeepAwakeSaysWhyItHeldNothing(t *testing.T) {
	s := NewMemoryStore()

	dead := claimed(t, s, time.Minute)
	s.Release(dead.ID, dead.Lease.Epoch)
	dead, _ = s.Load(dead.ID)
	if h := KeepAwake(s, dead); h.Held() || !errors.Is(h.Why(), ErrNoLease) {
		t.Fatalf("no live lease: held=%v, why=%v", h.Held(), h.Why())
	}

	h := KeepAwake(s, claimed(t, s, time.Minute))
	defer h.Release()
	switch refused := CanKeepAwake(); {
	case refused == nil && (!h.Held() || h.Why() != nil):
		t.Fatalf("this machine can be kept awake: held=%v, why=%v", h.Held(), h.Why())
	case refused != nil && (h.Held() || h.Why() == nil || errors.Is(h.Why(), ErrNoLease)):
		t.Fatalf("this machine refuses to be kept awake (%v): held=%v, why=%v", refused, h.Held(), h.Why())
	}
}

// lockHolder holds by locking a file. The kernel ends that lock with the
// process that took it, as it ends the platform's power request, and a parent
// can watch this one lock where the platform inhibitor is machine-wide and
// shared with every other program keeping the machine awake.
type lockHolder struct{ path string }

type lockHeld struct{ f *os.File }

func (l lockHolder) Hold(who, why string) (Held, error) {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	got, err := holdLock(f)
	if !got {
		f.Close()
		return nil, errors.Join(errors.New("hold lock already taken"), err)
	}
	return lockHeld{f}, nil
}

func (lockHeld) Done() <-chan struct{} { return nil }

func (h lockHeld) Release() error { return errors.Join(holdUnlock(h.f), h.f.Close()) }

// lockFree takes the lock at path and lets go, and reports whether it could. A
// lock nobody ever created is free.
func lockFree(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := holdLock(f)
	if err != nil {
		t.Fatal(err)
	}
	if got {
		if err := holdUnlock(f); err != nil {
			t.Fatal(err)
		}
	}
	return got
}

func TestHoldDiesWithHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hold.lock")
	child := exec.Command(os.Args[0], "-test.run=^TestHoldHelper$")
	child.Env = append(os.Environ(), "JOB_HOLD_HELPER="+path)
	out, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			child.Process.Kill()
			child.Wait()
		}
	}()
	said := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(out).ReadString('\n'); said <- line }()
	select {
	case line := <-said:
		if line != "held\n" {
			t.Fatalf("helper said %q", line)
		}
	case <-time.After(leaseEvent):
		t.Fatal("helper never reported its hold")
	}
	if lockFree(t, path) {
		t.Fatal("helper reports a hold, its lock is free")
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	child.Wait()
	waited = true
	for deadline := time.Now().Add(leaseEvent); !lockFree(t, path); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("holder killed: its hold outlived it")
		}
	}
}

func TestHoldHelper(t *testing.T) {
	path := os.Getenv("JOB_HOLD_HELPER")
	if path == "" {
		t.Skip()
	}
	s := NewMemoryStore()
	h := KeepAwakeVia(lockHolder{path}, s, claimed(t, s, time.Hour))
	if !h.Held() {
		fmt.Printf("not held: %v\n", h.Why())
		os.Exit(1)
	}
	os.Stdout.WriteString("held\n")
	select {}
}
