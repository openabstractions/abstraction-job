package acceptanceprovider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"unicode/utf8"

	cas "github.com/openabstractions/abstraction-cas/go"
)

var ErrIncompatibleStorage = errors.New("acceptance: incompatible storage")

// ErrLegacyOwnership means an unowned jobs directory contains entries. Their
// format and submitting authority have not been established. Managed/execution
// opens refuse before writes; selecting an explicit legacy provider preserves
// existing access. No caller mapping or work transfer is implied by this error.
// It also matches ErrIncompatibleStorage through errors.Is.
var ErrLegacyOwnership = fmt.Errorf("%w: unowned legacy jobs require an explicit ownership transfer", ErrIncompatibleStorage)

// CheckManaged checks the managed owner's format and executor profile without
// creating directories, locks, identities, or operations. A missing root is a
// compatible fresh destination. Journal/operation recovery remains Open's job.
// Hold AcquireHost across this check and replacement for an authoritative
// upgrade decision; an unlocked check is only a snapshot. Older writers that
// do not participate in that guard must be stopped by the orchestrator.
func CheckManaged(root string, executor Executor) error {
	if root == "" {
		return errors.New("acceptance: private root required")
	}
	version, profile, err := storageProfile(executor)
	if err != nil {
		return err
	}
	_, err = checkConfiguration(root, "", version, profile, true)
	return err
}

func storageProfile(executor Executor) (int, string, error) {
	if executor == nil {
		return 1, "", nil
	}
	profile := executor.Profile()
	if profile == "" || len(profile) > 256 || !utf8.ValidString(profile) {
		return 0, "", errors.New("acceptance: stable execution profile required")
	}
	return 2, profile, nil
}

func validateConfiguration(data []byte, owner string, version int, profile string, managed bool) (configuration, error) {
	var c configuration
	if !validConfigurationStrings(data) {
		return c, errors.New("acceptance: invalid owner configuration strings")
	}
	// Private owner metadata has exact, unique field names. In particular an
	// unsupported Version cannot disappear behind a later duplicate or alias.
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return c, errors.New("acceptance: invalid owner configuration")
	}
	seen := map[string]bool{}
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return c, err
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return c, errors.New("acceptance: ambiguous owner configuration")
		}
		seen[key] = true
		switch key {
		case "Version", "Owner", "Epoch", "RetentionMs", "Execution":
		default:
			return c, fmt.Errorf("%w: unsupported owner field %q", ErrIncompatibleStorage, key)
		}
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return c, err
		}
	}
	if err = strict(data, &c); err != nil {
		return c, err
	}
	if c.Version != version || c.Execution != profile || (!managed && c.Owner != owner) || c.Owner == "" || len(c.Owner) > 1024 || !utf8.ValidString(c.Owner) || c.Epoch == "" || c.RetentionMs != MinimumRetentionMs {
		return c, fmt.Errorf("%w: owner/version/execution profile mismatch", ErrIncompatibleStorage)
	}
	return c, nil
}

// encoding/json replaces malformed Unicode. Check original strings before that
// normalization; valid surrogate pairs and literal replacement characters stay
// valid. The JSON decoder subsequently checks the complete grammar.
func validConfigurationStrings(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	for i := 0; i < len(data); i++ {
		if data[i] != '"' {
			continue
		}
		i++
		for i < len(data) && data[i] != '"' {
			if data[i] != '\\' {
				i++
				continue
			}
			i++
			if i >= len(data) {
				return false
			}
			if data[i] != 'u' {
				i++
				continue
			}
			if i+4 >= len(data) {
				return false
			}
			value, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
			if err != nil {
				return false
			}
			i += 5
			if value >= 0xdc00 && value <= 0xdfff {
				return false
			}
			if value >= 0xd800 && value <= 0xdbff {
				if i+6 > len(data) || data[i] != '\\' || data[i+1] != 'u' {
					return false
				}
				low, err := strconv.ParseUint(string(data[i+2:i+6]), 16, 16)
				if err != nil || low < 0xdc00 || low > 0xdfff {
					return false
				}
				i += 6
			}
		}
	}
	return true
}

func checkConfiguration(root, owner string, version int, profile string, managed bool) (*configuration, error) {
	path := filepath.Join(root, "acceptance", "owner.json")
	if err := regularFile(path); err != nil {
		return nil, err
	}
	data, err := cas.ReadLimit(path, 4*MaxSpecBytes+16384)
	if err != nil {
		return nil, err
	}
	if data == nil {
		if managed || version == 2 {
			if err := freshRoot(root); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	c, err := validateConfiguration(data, owner, version, profile, managed)
	return &c, err
}

func freshRoot(root string) error {
	base, err := os.OpenRoot(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer base.Close()
	for _, dir := range []string{"jobs", filepath.Join("acceptance", "requests")} {
		info, err := base.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: %s must be a directory", ErrIncompatibleStorage, dir)
		}
		// OpenRoot requires a directory at the OS boundary. A FIFO or escaping
		// symlink substituted after Lstat cannot block this open or escape base.
		directory, err := base.OpenRoot(dir)
		if err != nil {
			return err
		}
		f, err := directory.Open(".")
		directory.Close()
		if err != nil {
			return err
		}
		entries, readErr := f.ReadDir(1)
		closeErr := f.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if len(entries) != 0 {
			if dir == "jobs" {
				return ErrLegacyOwnership
			}
			return fmt.Errorf("%w: unowned job or acceptance history requires explicit migration", ErrIncompatibleStorage)
		}
	}
	return nil
}
