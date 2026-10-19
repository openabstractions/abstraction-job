//go:build !windows

package acceptanceprovider

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// finalPath is dir with every symbolic link resolved.
func finalPath(dir string) (string, error) {
	return filepath.EvalSymlinks(dir)
}

// holdFinalPath is finalPath of dir; POSIX has no open-without-delete
// sharing, and release does nothing.
func holdFinalPath(dir string) (*heldLinkParent, error) {
	path, err := finalPath(dir)
	if err != nil {
		return nil, err
	}
	return &heldLinkParent{path: path, release: func() {}, link: func(source, leaf string) error {
		return os.Link(source, filepath.Join(path, leaf))
	}, outsideRoot: func(root string) (bool, error) {
		realRoot, err := finalPath(root)
		if err != nil {
			return false, err
		}
		return pathOutsideLinkRoot(path, realRoot), nil
	}}, nil
}

func linkDestinationLeaf(destination string) (linkLeaf, error) {
	return linkLeaf{name: filepath.Base(destination)}, nil
}

// otherVolume reports a hard link refused because the two names are on
// different file systems.
func otherVolume(err error) bool {
	return errors.Is(err, syscall.EXDEV)
}

// noHardLinks reports a file system that makes no hard links.
func noHardLinks(err error) bool {
	return errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP)
}
