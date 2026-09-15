package acceptanceprovider

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// HostActive reports whether a live host holds the store at path. It creates
// and writes nothing: it opens an existing acceptance/host.lock for reading and
// takes a shared lock for the instant of the probe. A missing root or lock file
// is a store no host has ever guarded, and nobody holds it. A host starting in
// that instant is refused with ErrHostActive once.
func HostActive(path string) (bool, error) {
	root, err := os.OpenRoot(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer root.Close()
	f, err := root.OpenFile("acceptance/host.lock", os.O_RDONLY, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("acceptance: host lock: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("acceptance: host lock: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("acceptance: host lock is not a regular file")
	}
	return probeHost(f)
}
