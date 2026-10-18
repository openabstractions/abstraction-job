//go:build !windows

package acceptanceprovider

import (
	"errors"
	"path/filepath"
	"syscall"
)

// finalPath is dir with every symbolic link resolved.
func finalPath(dir string) (string, error) {
	return filepath.EvalSymlinks(dir)
}

// holdFinalPath is finalPath of dir; POSIX has no open-without-delete
// sharing, and release does nothing.
func holdFinalPath(dir string) (string, func(), error) {
	path, err := finalPath(dir)
	return path, func() {}, err
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
