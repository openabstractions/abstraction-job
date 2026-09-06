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

const (
	reasonSimpleString  = 1
	powerSystemRequired = 1
)

type reasonContext struct {
	version uint32
	flags   uint32
	reason  *uint16
}

func keepAwake(who, why string) (func(), error) {
	text, err := syscall.UTF16PtrFromString(who + ": " + why)
	if err != nil {
		return nil, err
	}
	ctx := reasonContext{flags: reasonSimpleString, reason: text}
	h, _, err := powerCreateRequest.Call(uintptr(unsafe.Pointer(&ctx)))
	if syscall.Handle(h) == syscall.InvalidHandle {
		return nil, err
	}
	if r, _, err := powerSetRequest.Call(h, powerSystemRequired); r == 0 {
		syscall.CloseHandle(syscall.Handle(h))
		return nil, err
	}
	return func() {
		powerClearRequest.Call(h, powerSystemRequired)
		syscall.CloseHandle(syscall.Handle(h))
	}, nil
}
