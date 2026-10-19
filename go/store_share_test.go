//go:build windows || darwin || linux

package job

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	identity "github.com/openabstractions/abstraction-identity"
)

// Every record write takes the cas lock, which holds within one host, so a
// store rooted on a network share is refused when it is opened, with cas's
// reason, and nothing is created there (cas CAS-L2).
func TestAFileStoreOnANetworkShareIsRefused(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(shareSpelling(t, dir), "store")
	if s, err := NewFileStore(root); !errors.Is(err, identity.ErrNotLocalPath) {
		t.Fatalf("NewFileStore(%s) = %v, %v; want identity.ErrNotLocalPath", root, s, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("the refused store left %v in %s (%v)", entries, dir, err)
	}
}
