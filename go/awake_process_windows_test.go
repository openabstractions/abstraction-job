package job

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	holdLockFileEx   = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	holdUnlockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("UnlockFileEx")
)

// holdLock takes an exclusive byte-range lock without waiting and reports
// whether it got it. The kernel drops the lock when its handle closes, which a
// killed process does on its way out.
func holdLock(f *os.File) (bool, error) {
	var ov syscall.Overlapped
	r, _, err := holdLockFileEx.Call(f.Fd(), 3, 0, 1, 0, uintptr(unsafe.Pointer(&ov)))
	if r != 0 {
		return true, nil
	}
	if err == syscall.Errno(33) {
		return false, nil
	}
	return false, err
}

// holdUnlock lets go explicitly, since Windows may release a lock some time
// after its handle closes.
func holdUnlock(f *os.File) error {
	var ov syscall.Overlapped
	if r, _, err := holdUnlockFileEx.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&ov))); r == 0 {
		return err
	}
	return nil
}
