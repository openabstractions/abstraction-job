//go:build windows

package acceptanceprovider

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func otherVolumeError() error { return windows.ERROR_NOT_SAME_DEVICE }

func noHardLinksError() error { return windows.ERROR_INVALID_FUNCTION }

// JOB-A17: while LinkResult holds the destination's parent, that folder
// cannot be renamed away, so the resolved name is the one linked; a link
// made inside it still works, and release lets the folder go.
func TestHeldParentCannotBeReplacedWhileLinking(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "downloads")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	path, release, err := holdFinalPath(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parent, parent+"-moved"); err == nil {
		release()
		t.Fatal("a held parent was renamed")
	}
	source := filepath.Join(t.TempDir(), "result")
	if err := os.WriteFile(source, []byte("bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, filepath.Join(path, "model")); err != nil {
		release()
		t.Fatalf("a link inside the held parent: %v", err)
	}
	release()
	if err := os.Rename(parent, parent+"-moved"); err != nil {
		t.Fatalf("a released parent: %v", err)
	}
}

// directoryLink makes link a junction to target with cmd's mklink /J; both
// lie inside the test's own temporary folders.
func directoryLink(t *testing.T, target, link string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v %s", err, out)
	}
}
