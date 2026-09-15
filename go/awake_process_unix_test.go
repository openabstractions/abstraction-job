//go:build !windows

package job

import (
	"errors"
	"os"
	"syscall"
)

// holdLock takes an exclusive flock without waiting and reports whether it got
// it. The kernel drops the lock when the last descriptor closes, which a killed
// process does on its way out.
func holdLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return false, err
}

func holdUnlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
