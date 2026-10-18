package job

import (
	"syscall"
	"unsafe"
)

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	powerCreateRequest = kernel32.NewProc("PowerCreateRequest")
	powerSetRequest    = kernel32.NewProc("PowerSetRequest")
	powerClearRequest  = kernel32.NewProc("PowerClearRequest")
)

// Power request types (POWER_REQUEST_TYPE).
const (
	reasonSimpleString   = 1
	powerDisplayRequired = 0
	powerSystemRequired  = 1
)

type reasonContext struct {
	version uint32
	flags   uint32
	reason  *uint16
}

// powerCalls is the platform's power request API, a variable so a test can
// stand in for the platform and read which requests a hold sets.
type powerCalls interface {
	create(reason string) (syscall.Handle, error)
	set(h syscall.Handle, kind uintptr) error
	clear(h syscall.Handle, kind uintptr)
	close(h syscall.Handle)
}

var power powerCalls = kernelPower{}

type kernelPower struct{}

func (kernelPower) create(reason string) (syscall.Handle, error) {
	text, err := syscall.UTF16PtrFromString(reason)
	if err != nil {
		return syscall.InvalidHandle, err
	}
	ctx := reasonContext{flags: reasonSimpleString, reason: text}
	h, _, err := powerCreateRequest.Call(uintptr(unsafe.Pointer(&ctx)))
	if syscall.Handle(h) == syscall.InvalidHandle {
		return syscall.InvalidHandle, err
	}
	return syscall.Handle(h), nil
}

func (kernelPower) set(h syscall.Handle, kind uintptr) error {
	if r, _, err := powerSetRequest.Call(uintptr(h), kind); r == 0 {
		return err
	}
	return nil
}

func (kernelPower) clear(h syscall.Handle, kind uintptr) {
	//unchecked: clearing a request this hold set; the handle is closed next, which ends it in any case
	powerClearRequest.Call(uintptr(h), kind)
}

func (kernelPower) close(h syscall.Handle) {
	//unchecked: the request is already cleared; nothing is left to report
	syscall.CloseHandle(h)
}

// keepAwake takes one power request that keeps the system from idling into
// sleep. With display it also keeps the display on: on a Modern Standby
// machine the display turning off is the standby entry, and a system-required
// request alone did not hold it off (research/live-runs/2026-10-02.md,
// "Modern Standby": the idle timeout entered standby at 00:57:24 under such a
// hold, and a display-required one left it at 00:59:08 and held it through
// every run). Each request type set is cleared on release.
func keepAwake(who, why string, display bool) (func(), error) {
	h, err := power.create(who + ": " + why)
	if err != nil {
		return nil, err
	}
	kinds := []uintptr{powerSystemRequired}
	if display {
		kinds = append(kinds, powerDisplayRequired)
	}
	for i, kind := range kinds {
		if err := power.set(h, kind); err != nil {
			for _, done := range kinds[:i] {
				power.clear(h, done)
			}
			power.close(h)
			return nil, err
		}
	}
	return func() {
		for _, kind := range kinds {
			power.clear(h, kind)
		}
		power.close(h)
	}, nil
}
