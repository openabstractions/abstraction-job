package acceptanceprovider

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

var ErrHostActive = errors.New("acceptance: another host owns this store")

// AcquireHost reserves a service's private root before recovery or execution.
// The caller retains the returned handle until all admitted calls have
// drained. Admitted calls hold no lease and no epoch fences them. Its executor
// may be left running once its drain bound passed and YieldHeld(0) lapsed
// every lease the executor still holds [JOB-R9]; the store refuses that
// executor's later record writes by epoch. OS process death also releases
// ownership. The lock file stays in place: deleting it would allow a second
// lock inode for the same store.
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
	// Hosts starting together race to create the lock. On darwin a creator
	// that loses a plain O_CREAT race can fail with ENOENT
	// (golang/go#81246). O_EXCL makes the loser see ErrExist, and the loser
	// then opens the file the winner created.
	f, err := root.OpenFile("acceptance/host.lock", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if errors.Is(err, fs.ErrExist) {
		f, err = root.OpenFile("acceptance/host.lock", os.O_RDWR, 0600)
	}
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
