package acceptanceprovider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"unicode/utf8"
)

// AuthenticatedSubject is receiver-bound identity retained for an accepted job.
// Executable is the observed path used by local JOB-A1 scoping. Program is the
// receiving service's rights subject, which may be an MSIX package family.
type AuthenticatedSubject struct {
	AccountKind string
	Account     string
	Executable  string
	Program     string
}

// Binding is supplied by a receiving boundary, never decoded from a request.
// Origin is "local" for a native peer or "remote" for a receiver-owned mapping.
type Binding struct {
	Scope    string
	Subject  *AuthenticatedSubject
	Origin   string
	Evidence string
}

func (b Binding) valid() error {
	if b.Scope == "" || len(b.Scope) > MaxCallerScopeBytes || !utf8.ValidString(b.Scope) {
		return errors.New("acceptance: invalid binding scope")
	}
	if b.Subject == nil {
		if b.Origin != "" || b.Evidence != "" {
			return errors.New("acceptance: binding subject required")
		}
		return nil
	}
	s := b.Subject
	if (s.AccountKind != "windows" && s.AccountKind != "posix") || s.Account == "" || s.Program == "" || !utf8.ValidString(s.Account) || !utf8.ValidString(s.Program) || len(s.Account) > 1024 || len(s.Program) > 4096 || b.Origin != "local" && b.Origin != "remote" {
		return errors.New("acceptance: invalid binding subject")
	}
	if b.Origin == "local" {
		if b.Evidence != "" {
			return errors.New("acceptance: local binding has remote evidence")
		}
		if s.Executable == "" || !filepath.IsAbs(s.Executable) || filepath.Clean(s.Executable) != s.Executable {
			return errors.New("acceptance: invalid local executable")
		}
		scope, err := LocalSubjectScope(s.AccountKind, s.Account, s.Executable)
		if err != nil || scope != b.Scope {
			return errors.New("acceptance: local subject scope mismatch")
		}
	} else {
		if s.Executable != "" {
			return errors.New("acceptance: remote executable is not receiver proof")
		}
		key, err := hex.DecodeString(b.Evidence)
		if err != nil || len(key) != 32 {
			return errors.New("acceptance: remote certificate key evidence required")
		}
		var nonzero bool
		for _, b := range key {
			nonzero = nonzero || b != 0
		}
		if !nonzero {
			return errors.New("acceptance: remote certificate key evidence required")
		}
	}
	return nil
}

// LocalSubjectScope names one observed account and executable across restarts.
func LocalSubjectScope(kind, account, executable string) (string, error) {
	if (kind != "windows" && kind != "posix") || account == "" || executable == "" || !filepath.IsAbs(executable) {
		return "", errors.New("acceptance: proven account and absolute executable required")
	}
	data, err := json.Marshal([]string{"owner-program@1", kind, account, filepath.Clean(executable)})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return "owner-program@1:" + hex.EncodeToString(digest[:]), nil
}
