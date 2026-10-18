//go:build windows

package acceptanceprovider

import (
	"errors"
	"strings"

	"golang.org/x/sys/windows"
)

// finalPath is the path Windows resolves dir to, through every junction,
// symbolic link and short name (GetFinalPathNameByHandle).
func finalPath(dir string) (string, error) {
	path, release, err := openFinalPath(dir, windows.FILE_SHARE_DELETE)
	if err != nil {
		return "", err
	}
	release()
	return path, nil
}

// holdFinalPath is finalPath of dir, with dir held open without delete
// sharing until release: while held, the folder at that final path cannot
// be renamed, removed or replaced, so the name resolved is the name linked.
func holdFinalPath(dir string) (string, func(), error) {
	return openFinalPath(dir, 0)
}

func openFinalPath(dir string, deleteShare uint32) (string, func(), error) {
	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return "", nil, err
	}
	// The open asks for FILE_LIST_DIRECTORY: Windows records and enforces
	// a share mode only for a handle that holds data access. Whatever
	// answers the open gets an identification-level quality of service, so
	// it cannot act as the provider.
	h, err := windows.CreateFile(name, windows.FILE_LIST_DIRECTORY, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|deleteShare, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
	if err != nil {
		return "", nil, err
	}
	release := func() {
		//unchecked: closing a directory query handle reports nothing a caller acts on
		_ = windows.CloseHandle(h)
	}
	path, err := handlePath(h)
	if err != nil {
		release()
		return "", nil, err
	}
	return path, release, nil
}

// handlePath is the DOS form of an open handle's final path.
func handlePath(h windows.Handle) (string, error) {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0 /* VOLUME_NAME_DOS, FILE_NAME_NORMALIZED */)
	if err != nil {
		return "", err
	}
	if int(n) >= len(buf) {
		return "", errors.New("acceptance: final path too long")
	}
	path := windows.UTF16ToString(buf[:n])
	if strings.HasPrefix(path, `\\?\UNC\`) {
		return `\\` + path[len(`\\?\UNC\`):], nil
	}
	return strings.TrimPrefix(path, `\\?\`), nil
}

// otherVolume reports a hard link refused because the two names are on
// different volumes.
func otherVolume(err error) bool {
	return errors.Is(err, windows.ERROR_NOT_SAME_DEVICE)
}

// noHardLinks reports a file system that makes no hard links, such as FAT.
func noHardLinks(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_FUNCTION) || errors.Is(err, windows.ERROR_NOT_SUPPORTED)
}
