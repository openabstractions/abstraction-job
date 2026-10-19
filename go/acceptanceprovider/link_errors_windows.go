//go:build windows

package acceptanceprovider

import (
	"context"
	"errors"
	"os"
	"strings"
	"unsafe"

	"github.com/openabstractions/abstraction-identity/fileid"
	"golang.org/x/sys/windows"
)

// holdFinalPath resolves with fileid, then pins that SAME object before
// checking its captured identity/path. Every admission and effect rechecks the
// current object. A moved snapshot is refused, including Relate's Outside case.
func holdFinalPath(dir string) (*heldLinkParent, error) {
	held, err := fileid.Open(context.Background(), fileid.Self, dir, fileid.Options{})
	if err != nil {
		return nil, err
	}
	pin, err := pinLinkDirectory(held)
	if err != nil {
		return nil, errors.Join(err, held.Close())
	}
	releases := []func(){func() { _ = windows.CloseHandle(pin); _ = held.Close() }}
	var providerCurrent func() error
	current := func() error { return currentLinkDirectory(held, pin) }
	return &heldLinkParent{path: held.FileID().Path(), key: held.FileID().Key(), release: func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}, outsideRoot: func(root string) (bool, error) {
		provider, err := fileid.Open(context.Background(), fileid.Self, root, fileid.Options{})
		if err != nil {
			return false, err
		}
		rootPin, err := pinLinkDirectory(provider)
		if err != nil {
			return false, errors.Join(err, provider.Close())
		}
		releases = append(releases, func() { _ = windows.CloseHandle(rootPin); _ = provider.Close() })
		providerCurrent = func() error { return currentLinkDirectory(provider, rootPin) }
		if err := errors.Join(current(), providerCurrent()); err != nil {
			return false, err
		}
		relation, err := held.Relate(provider)
		if err != nil {
			return false, err
		}
		if err := errors.Join(current(), providerCurrent()); err != nil {
			return false, err
		}
		return relation.Kind == fileid.RelationOutside, nil
	}, link: func(source, leaf string) error {
		if err := current(); err != nil {
			return err
		}
		if providerCurrent != nil {
			if err := providerCurrent(); err != nil {
				return err
			}
		}
		return linkAtHeldParent(pin, source, leaf)
	}}, nil
}

// Empty-name NtCreateFile is fileid's existing same-object reopen pattern.
// No pathname is resolved here. Data access makes the no-DELETE share pin real.
func pinLinkDirectory(held fileid.Handle) (windows.Handle, error) {
	if !held.FileID().IsDir() {
		return 0, errors.New("acceptance: destination parent is no directory")
	}
	empty, err := windows.NewNTUnicodeString("")
	if err != nil {
		return 0, err
	}
	qos := windows.SECURITY_QUALITY_OF_SERVICE{Length: uint32(unsafe.Sizeof(windows.SECURITY_QUALITY_OF_SERVICE{})), ImpersonationLevel: windows.SecurityIdentification}
	oa := windows.OBJECT_ATTRIBUTES{Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: windows.Handle(held.File().Fd()), ObjectName: empty, SecurityQoS: &qos}
	var pin windows.Handle
	err = windows.NtCreateFile(&pin, windows.FILE_LIST_DIRECTORY|windows.SYNCHRONIZE, &oa, &windows.IO_STATUS_BLOCK{}, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_FOR_BACKUP_INTENT, 0, 0)
	if status, ok := err.(windows.NTStatus); ok {
		err = status.Errno()
	}
	if err != nil {
		return 0, err
	}
	if err := currentLinkDirectory(held, pin); err != nil {
		return 0, errors.Join(err, windows.CloseHandle(pin))
	}
	return pin, nil
}

// Compare same-object query data without reopening a path. Preserve every
// actual filename character while normalizing FileID.Path's display prefix.
func currentLinkDirectory(held fileid.Handle, pin windows.Handle) error {
	key, err := fileid.FileKeyOf(held.File())
	if err != nil {
		return err
	}
	path, err := handlePath(pin)
	if err != nil {
		return err
	}
	if key != held.FileID().Key() || path != strings.TrimPrefix(held.FileID().Path(), `\\?\`) {
		return errors.New("acceptance: held link directory moved or changed")
	}
	return nil
}

// handlePath is the existing native query owner; it never reopens a name.
func handlePath(h windows.Handle) (string, error) {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
	if err != nil {
		return "", err
	}
	if n == 0 || int(n) >= len(buf) {
		return "", errors.New("acceptance: final path too long")
	}
	path := windows.UTF16ToString(buf[:n])
	if strings.HasPrefix(path, `\\?\UNC\`) {
		return `\\` + path[len(`\\?\UNC\`):], nil
	}
	return strings.TrimPrefix(path, `\\?\`), nil
}

// ResolveLeaf owns ordinary Win32 trimming, extended-path trailing-dot/space
// spelling and existing-entry aliases. The native helper receives that leaf.
func linkDestinationLeaf(destination string) (linkLeaf, error) {
	leaf, err := fileid.ResolveLeaf(context.Background(), fileid.Self, destination)
	if err != nil {
		return linkLeaf{}, err
	}
	return linkLeaf{name: leaf.Name(), folderKey: leaf.Folder().Key()}, nil
}

// linkAtHeldParent follows the SDK's native Linkat operation. FileLinkInformation
// class 11 takes a byte-counted UTF-16 leaf relative to RootDirectory, with
// ReplaceIfExists false. The class is explicit: x/sys's similarly named
// FileLinkInformation constant is class 72, a different information layout.
// https://learn.microsoft.com/windows-hardware/drivers/ddi/ntifs/ns-ntifs-_file_link_information
func linkAtHeldParent(parent windows.Handle, source, leaf string) (err error) {
	if err := fileid.SafeName(strings.TrimRight(leaf, ". ")); err != nil {
		return &os.LinkError{Op: "link", Old: source, New: leaf, Err: windows.ERROR_INVALID_NAME}
	}
	name, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(name, windows.FILE_WRITE_ATTRIBUTES|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
	if err != nil {
		return &os.LinkError{Op: "link", Old: source, New: leaf, Err: err}
	}
	defer func() {
		if closeErr := windows.CloseHandle(h); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	var info windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return errors.New("acceptance: named result is no regular file")
	}
	encoded, err := windows.UTF16FromString(leaf)
	if err != nil {
		return err
	}
	var header struct {
		ReplaceIfExists uint32
		RootDirectory   windows.Handle
		FileNameLength  uint32
		FileName        [1]uint16
	}
	// Allocate the complete ABI buffer including its trailing filename and
	// native alignment. FileNameLength excludes the terminating NUL.
	offset := int(unsafe.Offsetof(header.FileName))
	buffer := make([]byte, int(unsafe.Sizeof(header))+len(encoded)*2)
	data := (*struct {
		ReplaceIfExists uint32
		RootDirectory   windows.Handle
		FileNameLength  uint32
	})(unsafe.Pointer(&buffer[0]))
	data.RootDirectory = parent
	data.FileNameLength = uint32((len(encoded) - 1) * 2)
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&buffer[offset])), len(encoded)), encoded)
	const fileLinkInformation = 11
	err = windows.NtSetInformationFile(h, &windows.IO_STATUS_BLOCK{}, &buffer[0], uint32(len(buffer)), fileLinkInformation)
	if status, ok := err.(windows.NTStatus); ok {
		err = status.Errno()
	}
	if err != nil {
		return &os.LinkError{Op: "link", Old: source, New: leaf, Err: err}
	}
	return nil
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
