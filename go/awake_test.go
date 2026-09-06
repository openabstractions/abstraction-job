package job

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"
)

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

func TestHoldFollowsLease(t *testing.T) {
	if inhibited() {
		t.Skip("something else already holds the machine awake; the observer cannot tell ours apart")
	}
	s := NewMemoryStore()

	r := claimed(t, s, time.Minute)
	h := KeepAwake(s, r)
	if !h.Held() || !inhibited() {
		t.Fatal("claimed and running: not held")
	}
	h.Release()
	if h.Held() || inhibited() {
		t.Fatal("released by the holder: still held")
	}

	r = claimed(t, s, time.Minute)
	h = KeepAwake(s, r)
	s.Release(r.ID, r.Lease.Epoch)
	if !settles(h) {
		t.Fatal("lease released in the store: still held")
	}

	r = claimed(t, s, time.Minute)
	h = KeepAwake(s, r)
	s.Update(r.ID, r.Lease.Epoch, func(rr *Record) error { rr.State = StateFailed; return nil })
	if !settles(h) {
		t.Fatal("job terminal: still held")
	}

	r = claimed(t, s, 300*time.Millisecond)
	h = KeepAwake(s, r)
	if !settles(h) {
		t.Fatal("lease lapsed: still held")
	}

	r = claimed(t, s, time.Minute)
	s.Release(r.ID, r.Lease.Epoch)
	r, _ = s.Load(r.ID)
	if h = KeepAwake(s, r); h.Held() {
		t.Fatal("no live lease: held")
	}
	if inhibited() {
		t.Fatal("a hold outlived its test")
	}
}

func settles(h *Hold) bool {
	deadline := time.Now().Add(5 * time.Second)
	for h.Held() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	return !h.Held() && !inhibited()
}

func TestHoldDiesWithHolder(t *testing.T) {
	if inhibited() {
		t.Skip("something else already holds the machine awake; the observer cannot tell ours apart")
	}
	child := exec.Command(os.Args[0], "-test.run=TestHoldHelper")
	child.Env = append(os.Environ(), "JOB_HOLD_HELPER=1")
	out, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	if line, _ := bufio.NewReader(out).ReadString('\n'); line != "held\n" {
		child.Process.Kill()
		t.Fatalf("helper said %q", line)
	}
	if !inhibited() {
		child.Process.Kill()
		t.Fatal("helper holds, machine not inhibited")
	}
	child.Process.Kill()
	child.Wait()
	if inhibited() {
		t.Fatal("holder killed: still inhibited")
	}
}

func TestHoldHelper(t *testing.T) {
	if os.Getenv("JOB_HOLD_HELPER") == "" {
		t.Skip()
	}
	s := NewMemoryStore()
	KeepAwake(s, claimed(t, s, time.Hour))
	os.Stdout.WriteString("held\n")
	select {}
}
