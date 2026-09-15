//go:build !windows

package acceptanceprovider

import (
	"errors"
	"os"
	"syscall"
)

// probeHost takes a shared flock, which a read-only descriptor may hold and
// which conflicts only with a host's exclusive one.
func probeHost(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
