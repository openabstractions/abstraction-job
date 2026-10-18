package job

import (
	"fmt"
	"slices"
	"sync"
	"syscall"
	"testing"
)

// fakePower stands in for the platform's power request API and records each
// call in order.
type fakePower struct {
	mu    sync.Mutex
	calls []string
	next  syscall.Handle
}

func (f *fakePower) create(reason string) (syscall.Handle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	f.calls = append(f.calls, "create "+reason)
	return f.next, nil
}

func (f *fakePower) set(h syscall.Handle, kind uintptr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("set %d %s", h, requestName(kind)))
	return nil
}

func (f *fakePower) clear(h syscall.Handle, kind uintptr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("clear %d %s", h, requestName(kind)))
}

func (f *fakePower) close(h syscall.Handle) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("close %d", h))
}

func requestName(kind uintptr) string {
	switch kind {
	case powerDisplayRequired:
		return "display"
	case powerSystemRequired:
		return "system"
	}
	return fmt.Sprint(kind)
}

func withFakePower(t *testing.T) *fakePower {
	t.Helper()
	fake := &fakePower{}
	saved := power
	power = fake
	t.Cleanup(func() { power = saved })
	return fake
}

// RES-A1, RES-G5: the hold the runtime keeps for GPU work also requires the
// display, since on a Modern Standby machine the display turning off is the
// standby entry; a system-required request alone let this machine enter
// standby at 00:57:24 on 2026-10-02 (research/live-runs/2026-10-02.md). The
// display is required only while the hold lasts, and every other hold, a
// download's, requires the system alone.
func TestTheGPUHoldRequiresTheDisplayAndOnlyWhileHeld(t *testing.T) {
	fake := withFakePower(t)
	held, err := PlatformGPU.Hold("openabstractions", "1 wake lease(s) of abstraction.resource")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"create openabstractions: 1 wake lease(s) of abstraction.resource", "set 1 system", "set 1 display"}; !slices.Equal(fake.calls, want) {
		t.Fatalf("a GPU hold made %q, want %q", fake.calls, want)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"clear 1 system", "clear 1 display", "close 1"}; !slices.Equal(fake.calls[3:], want) {
		t.Fatalf("releasing a GPU hold made %q, want %q", fake.calls[3:], want)
	}

	fake.calls = nil
	held, err = Platform.Hold("download", "job 1")
	if err != nil {
		t.Fatal(err)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(fake.calls, func(c string) bool { return c == "set 2 display" }) {
		t.Fatalf("a download's hold required the display: %q", fake.calls)
	}
	if want := []string{"create download: job 1", "set 2 system", "clear 2 system", "close 2"}; !slices.Equal(fake.calls, want) {
		t.Fatalf("a download's hold made %q, want %q", fake.calls, want)
	}
}
