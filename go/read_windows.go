package job

import (
	"io"
	"os"
	"path/filepath"
	"sync"
)

// The SMB redirector keeps a closed file's server handle open and hands it to
// the next open it can serve, so a record the far side renamed over is read
// from the old inode until that handle idles out: 154 s in a supervisor
// polling every 10 s, indefinitely at 1 s, and a fresh process on the same PC
// reads through the same handle. Only an open the cached handle cannot serve
// sends a new CREATE, so consecutive opens of one record ask for different
// access. Measured in research/smb-freshness/RESULTS.txt.
var lastOpenWasRW sync.Map

func readFile(path string) ([]byte, error) {
	was, _ := lastOpenWasRW.LoadOrStore(path, false)
	lastOpenWasRW.Store(path, !was.(bool))
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	name := filepath.Base(path)
	f, err := root.OpenFile(name, os.O_RDWR, 0)
	if was.(bool) || err != nil {
		f, err = root.Open(name)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
