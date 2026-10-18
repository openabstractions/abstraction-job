//go:build !windows

package acceptanceprovider

import (
	"os"
	"syscall"
	"testing"
)

func otherVolumeError() error { return syscall.EXDEV }

func noHardLinksError() error { return syscall.EOPNOTSUPP }

// directoryLink makes link a symbolic link to the directory target; both lie
// inside the test's own temporary folders.
func directoryLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
