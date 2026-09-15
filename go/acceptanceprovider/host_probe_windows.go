package acceptanceprovider

import (
	"os"
	"syscall"
	"unsafe"
)

var hostUnlockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("UnlockFileEx")

// probeHost takes a shared lock on the byte lockHost holds exclusively. A
// read-only handle may lock, and a shared lock conflicts only with a host.
func probeHost(f *os.File) (bool, error) {
	var ov syscall.Overlapped
	// LOCKFILE_FAIL_IMMEDIATELY without LOCKFILE_EXCLUSIVE_LOCK is shared.
	result, _, err := hostLockFileEx.Call(f.Fd(), 1, 0, 1, 0, uintptr(unsafe.Pointer(&ov)))
	if result == 0 {
		if err == syscall.Errno(33) {
			return true, nil
		}
		return false, err
	}
	var un syscall.Overlapped
	if result, _, err = hostUnlockFileEx.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&un))); result == 0 {
		return false, err
	}
	return false, nil
}
