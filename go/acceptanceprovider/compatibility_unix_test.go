//go:build linux || darwin

package acceptanceprovider

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckManagedRejectsNonDirectoryWithoutBlocking(t *testing.T) {
	for _, relative := range []string{"jobs", filepath.Join("acceptance", "requests")} {
		root := t.TempDir()
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mkfifo(path, 0600); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- CheckManaged(root, nil) }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("FIFO accepted")
			}
		case <-time.After(time.Second):
			fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK, 0)
			if err == nil {
				unix.Close(fd)
			}
			t.Fatal("preflight blocked on FIFO")
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), path); err != nil {
			t.Fatal(err)
		}
		if err := CheckManaged(root, nil); err == nil {
			t.Fatal("symlink directory accepted as fresh")
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := CheckManaged(root, nil); err != nil {
			t.Fatal("empty directory refused", err)
		}
	}
}
