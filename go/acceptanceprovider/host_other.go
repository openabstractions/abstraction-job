//go:build !windows

package acceptanceprovider

import (
	"errors"
	"os"
	"syscall"
)

func lockHost(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrHostActive
	}
	return err
}
