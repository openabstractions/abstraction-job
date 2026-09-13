package acceptanceprovider

import (
	"errors"
	"fmt"
	"io"
	"os"
)

var ErrHostActive = errors.New("acceptance: another host owns this store")

// AcquireHost reserves a service's private root before recovery or execution.
// The caller retains the returned handle until all admitted calls and workers
// have drained. OS process death also releases ownership. The lock file stays
// in place: deleting it would allow a second lock inode for the same store.
// Direct provider users retain responsibility for their own lifecycle. Older
// hosts that predate this guard must be stopped before replacement or migration.
func AcquireHost(path string) (io.Closer, error) {
	if path == "" {
		return nil, errors.New("acceptance: private root required")
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err = root.MkdirAll("acceptance", 0700); err != nil {
		return nil, err
	}
	f, err := root.OpenFile("acceptance/host.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("acceptance: host lock must be a regular file")
	}
	if err == nil {
		err = lockHost(f)
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("acceptance host: %w", err)
	}
	return f, nil
}
