package acceptanceprovider

import (
	"os"
	"syscall"
	"unsafe"
)

var hostLockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")

func lockHost(f *os.File) error {
	var ov syscall.Overlapped
	// LOCKFILE_EXCLUSIVE_LOCK | LOCKFILE_FAIL_IMMEDIATELY, byte range [0,1).
	result, _, err := hostLockFileEx.Call(f.Fd(), 3, 0, 1, 0, uintptr(unsafe.Pointer(&ov)))
	if result != 0 {
		return nil
	}
	if err == syscall.Errno(33) {
		return ErrHostActive
	}
	return err
}
