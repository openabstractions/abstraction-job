package acceptanceprovider

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	job "github.com/openabstractions/abstraction-job/go"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

// linkExecutor keeps each complete result at results/<operation id> under
// the provider root, as the download kind does.
type linkExecutor struct {
	testExecutor
	root string
}

func (e linkExecutor) ReadOperationResult(_ string, record *job.Record, offset, maxBytes int64) ([]byte, int64, error) {
	data, err := os.ReadFile(filepath.Join(e.root, "results", record.ID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, ErrResultLost
	}
	if err != nil {
		return nil, 0, err
	}
	if offset > int64(len(data)) {
		return nil, 0, ErrResultRange
	}
	return data[offset:min(int64(len(data)), offset+maxBytes)], int64(len(data)), nil
}

func (linkExecutor) OperationResultName(record *job.Record) (string, error) {
	return "results/" + record.ID, nil
}

// linkFixture is a provider holding one complete result for a local caller.
type linkFixture struct {
	p       *Provider
	id      api.RequestIdentity
	receipt api.Receipt
	result  string
	local   Binding
	body    []byte
}

func localBinding(t *testing.T) Binding {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "setup.exe")
	kind, account := "posix", "1000"
	if runtime.GOOS == "windows" {
		kind, account = "windows", "S-1-5-21-1-2-3-1001"
	}
	scope, err := LocalSubjectScope(kind, account, executable)
	if err != nil {
		t.Fatal(err)
	}
	return Binding{Scope: scope, Origin: "local", Peer: &identity.Peer{},
		Subject: &AuthenticatedSubject{AccountKind: kind, Account: account, Executable: executable, Program: executable}}
}

func newLinkFixture(t *testing.T, complete bool) *linkFixture {
	t.Helper()
	root := t.TempDir()
	p, err := OpenWithExecutor(root, "logical-owner", linkExecutor{testExecutor: testExecutor{profile: "result-v1"}, root: root})
	if err != nil {
		t.Fatal(err)
	}
	// Tests decide the caller's authority themselves.
	p.SetLinkAuthority(func(*identity.Peer, string) error { return nil })
	f := &linkFixture{p: p, local: localBinding(t), body: []byte("verified model bytes")}
	s := submission(p, "linked")
	f.id = s.Identity
	f.receipt = accept(t, p.BindBinding(f.local), s)
	if !complete {
		return f
	}
	claim, err := p.store.Claim(f.receipt.OperationID, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	f.result = filepath.Join(root, "results", f.receipt.OperationID)
	if err := os.MkdirAll(filepath.Dir(f.result), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.result, f.body, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.store.Update(claim.ID, claim.Lease.Epoch, func(record *job.Record) error { record.State = job.StateComplete; return nil }); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *linkFixture) link(t *testing.T, b Binding, destination string) api.ResultLink {
	t.Helper()
	r, err := f.p.BindOperationsBinding(b).LinkResult(f.id, destination)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// JOB-A17: a complete result gets a second name at the destination; it is
// the same file, the receipt and size come back, and the result stays
// readable by its own name.
func TestLinkResultMakesASecondNameForTheSameFile(t *testing.T) {
	f := newLinkFixture(t, true)
	destination := filepath.Join(t.TempDir(), "model.safetensors")
	r := f.link(t, f.local, destination)
	if r.Outcome != api.LinkOutcomeLinked || r.Receipt == nil || r.Receipt.OperationID != f.receipt.OperationID || r.Size != int64(len(f.body)) {
		t.Fatalf("link %+v", r)
	}
	stored, err := os.Stat(f.result)
	if err != nil {
		t.Fatal(err)
	}
	linked, err := os.Stat(destination)
	if err != nil || !os.SameFile(stored, linked) {
		t.Fatalf("the destination is not the stored result: %v", err)
	}
	read, err := f.p.BindOperations(f.local.Scope).ReadResult(f.id, 0, MaxResultBytes)
	if err != nil || read.Chunk == nil || !bytes.Equal(read.Chunk.Data, f.body) {
		t.Fatalf("the result is not readable by its own name after linking: %+v %v", read, err)
	}
	// A second link to the same name finds it present and changes nothing.
	if again := f.link(t, f.local, destination); again.Outcome != api.LinkOutcomeExists || again.Receipt != nil || again.Size != 0 {
		t.Fatalf("relink %+v", again)
	}
}

// JOB-A17: no hard link is ever replaced by a copy. Another volume and a file
// system without hard links are refused by name, and nothing is created.
func TestLinkResultRefusesWhatAHardLinkCannotReach(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want api.LinkOutcome
	}{{otherVolumeError(), api.LinkOutcomeOtherVolume}, {noHardLinksError(), api.LinkOutcomeNoHardLinks}} {
		f := newLinkFixture(t, true)
		saved := linkFile
		linkFile = func(string, string) error { return &os.LinkError{Op: "link", Err: tc.err} }
		destination := filepath.Join(t.TempDir(), "model.safetensors")
		r := f.link(t, f.local, destination)
		linkFile = saved
		if r.Outcome != tc.want {
			t.Fatalf("%v: %+v", tc.err, r)
		}
		if _, err := os.Lstat(destination); !os.IsNotExist(err) {
			t.Fatalf("%v: a refused link left %s: %v", tc.err, destination, err)
		}
	}
}

// JOB-A17: an existing destination stays as it was; the destination must be
// an absolute clean path outside the provider root whose parent exists.
func TestLinkResultRefusesADestinationItMayNotTake(t *testing.T) {
	f := newLinkFixture(t, true)
	present := filepath.Join(t.TempDir(), "present")
	if err := os.WriteFile(present, []byte("mine"), 0600); err != nil {
		t.Fatal(err)
	}
	if r := f.link(t, f.local, present); r.Outcome != api.LinkOutcomeExists {
		t.Fatalf("existing destination %+v", r)
	}
	if data, _ := os.ReadFile(present); string(data) != "mine" {
		t.Fatal("an existing destination changed")
	}
	dir := t.TempDir()
	for _, destination := range []string{"", "relative/model", filepath.Join(dir, "missing", "model"),
		dir + string(filepath.Separator) + "." + string(filepath.Separator) + "model", filepath.Join(f.p.root, "copy")} {
		if r := f.link(t, f.local, destination); r.Outcome != api.LinkOutcomeInvalid {
			t.Fatalf("destination %q: %+v", destination, r)
		}
	}
}

// JOB-A17: the destination's parent is judged by its final path, so a
// junction or link into the provider root names the provider's own layout
// and is refused; nothing is created there.
func TestLinkResultRefusesAJunctionIntoTheProviderRoot(t *testing.T) {
	f := newLinkFixture(t, true)
	results := filepath.Join(f.p.root, "results")
	before, err := os.ReadDir(results)
	if err != nil {
		t.Fatal(err)
	}
	junction := filepath.Join(t.TempDir(), "into-root")
	directoryLink(t, results, junction)
	if r := f.link(t, f.local, filepath.Join(junction, "copy")); r.Outcome != api.LinkOutcomeInvalid {
		t.Fatalf("a junction into the provider root linked: %+v", r)
	}
	if after, _ := os.ReadDir(results); len(after) != len(before) {
		t.Fatalf("a refused link created %d names in the provider root", len(after)-len(before))
	}
	// A destination through a junction to an ordinary folder is linked at
	// that folder's final path.
	outside := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	directoryLink(t, outside, elsewhere)
	if r := f.link(t, f.local, filepath.Join(elsewhere, "model")); r.Outcome != api.LinkOutcomeLinked {
		t.Fatalf("a junction to an ordinary folder: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(outside, "model")); err != nil {
		t.Fatalf("the link is not at the folder's final path: %v", err)
	}
}

// JOB-A17: only a caller the receiver proved on this machine may ask for a
// link, under its own authority to create the path; a remote binding keeps
// reading with ReadResult.
func TestLinkResultIsLocalAndUnderTheCallersAuthority(t *testing.T) {
	f := newLinkFixture(t, true)
	destination := filepath.Join(t.TempDir(), "model.safetensors")
	remote := Binding{Scope: f.local.Scope}
	if r, _ := f.p.BindOperations(f.local.Scope).LinkResult(f.id, destination); r.Outcome != api.LinkOutcomeRemote {
		t.Fatalf("a binding without local peer evidence linked: %+v", r)
	}
	if r := f.link(t, remote, destination); r.Outcome != api.LinkOutcomeRemote {
		t.Fatalf("a remote binding linked: %+v", r)
	}
	f.p.SetLinkAuthority(func(*identity.Peer, string) error { return errNotCallerAuthority })
	if r := f.link(t, f.local, destination); r.Outcome != api.LinkOutcomeForbidden {
		t.Fatalf("a caller without create authority linked: %+v", r)
	}
	// The default decision needs the caller's kernel-proven account.
	f.p.SetLinkAuthority(nil)
	if r := f.link(t, f.local, destination); r.Outcome != api.LinkOutcomeForbidden {
		t.Fatalf("an unproven caller linked: %+v", r)
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatal("a refused caller created the destination")
	}
	if r, _ := f.p.BindOperations("").LinkResult(f.id, destination); r.Outcome != api.LinkOutcomeForbidden {
		t.Fatalf("no scope: %+v", r)
	}
	if r, _ := f.p.BindOperationsBinding(localBinding(t)).LinkResult(f.id, destination); r.Outcome != api.LinkOutcomeUnknown {
		t.Fatalf("another caller's operation: %+v", r)
	}
}

// R2: a caller without the authority to create its destination is refused
// before anything reads or opens the destination, its parent or its final
// path, even when it names the provider's own layout; with that authority
// the same destination is refused as the provider's layout.
func TestLinkResultChecksTheCallerBeforeTouchingItsDestination(t *testing.T) {
	f := newLinkFixture(t, true)
	touched := 0
	savedStat, savedLstat, savedHold, savedLink := statPath, lstatPath, holdFinal, linkFile
	t.Cleanup(func() { statPath, lstatPath, holdFinal, linkFile = savedStat, savedLstat, savedHold, savedLink })
	statPath = func(p string) (os.FileInfo, error) { touched++; return savedStat(p) }
	lstatPath = func(p string) (os.FileInfo, error) { touched++; return savedLstat(p) }
	holdFinal = func(p string) (string, func(), error) { touched++; return savedHold(p) }
	linkFile = func(a, b string) error { touched++; return savedLink(a, b) }
	refused := []string{filepath.Join(f.p.root, "results", "copy"), filepath.Join(t.TempDir(), "model")}
	if runtime.GOOS == "windows" {
		refused = append(refused, `\\192.0.2.1\share\model`)
	}
	f.p.SetLinkAuthority(func(*identity.Peer, string) error { return errNotCallerAuthority })
	for _, destination := range refused {
		if r := f.link(t, f.local, destination); r.Outcome != api.LinkOutcomeForbidden || touched != 0 {
			t.Fatalf("a refused caller naming %q: %+v after %d destination reads", destination, r, touched)
		}
	}
	// The default authority refuses a caller with no kernel-proven account,
	// as it refuses a restricted token, before any read either.
	f.p.SetLinkAuthority(nil)
	if r := f.link(t, f.local, refused[0]); r.Outcome != api.LinkOutcomeForbidden || touched != 0 {
		t.Fatalf("an unproven caller: %+v after %d destination reads", r, touched)
	}
	f.p.SetLinkAuthority(func(*identity.Peer, string) error { return nil })
	if r := f.link(t, f.local, refused[0]); r.Outcome != api.LinkOutcomeInvalid {
		t.Fatalf("the provider's own layout: %+v", r)
	}
	if runtime.GOOS == "windows" {
		before := touched
		if r := f.link(t, f.local, refused[2]); r.Outcome != api.LinkOutcomeOtherVolume || touched != before {
			t.Fatalf("a share destination: %+v after %d destination reads", r, touched-before)
		}
	}
}

// JOB-A17: an unfinished operation has nothing to link, and a lost result is
// unavailable.
func TestLinkResultNeedsACompleteResult(t *testing.T) {
	f := newLinkFixture(t, false)
	if r := f.link(t, f.local, filepath.Join(t.TempDir(), "model")); r.Outcome != api.LinkOutcomeNotReady {
		t.Fatalf("pending: %+v", r)
	}
	done := newLinkFixture(t, true)
	if err := os.Remove(done.result); err != nil {
		t.Fatal(err)
	}
	if r := done.link(t, done.local, filepath.Join(t.TempDir(), "model")); r.Outcome != api.LinkOutcomeUnavailable {
		t.Fatalf("lost: %+v", r)
	}
}

// SameAuthority's rule: the caller's account equals the provider's, at no
// lower integrity, with an unrestricted token.
func TestSameAuthorityComparesAccountAndIntegrity(t *testing.T) {
	own := principal{account: "S-1-5-21-1", integrity: 0x2000}
	for _, tc := range []struct {
		caller principal
		ok     bool
	}{
		{principal{account: "S-1-5-21-1", integrity: 0x2000}, true},
		{principal{account: "S-1-5-21-1", integrity: 0x3000}, true},
		{principal{account: "S-1-5-21-1", integrity: 0x1000}, false},
		{principal{account: "S-1-5-21-2", integrity: 0x2000}, false},
		{principal{account: "S-1-5-21-1", integrity: 0x2000, restricted: true}, false},
		{principal{integrity: 0x2000}, false},
	} {
		if err := sameOrLowerAuthority(tc.caller, own); (err == nil) != tc.ok {
			t.Errorf("%+v: %v", tc.caller, err)
		}
	}
	if user, err := selfUser(); err != nil {
		t.Fatalf("the provider's own principal: %v", err)
	} else if _, err := userPrincipal(user); err != nil {
		t.Fatalf("the provider's own principal: %+v %v", user, err)
	}
}

// JOB-A17: a provider whose own integrity level is unknown links for no one,
// and a caller whose level is unknown, or whose token is restricted, gets no
// link; RID 0 read from an unknown label never stands for a level.
func TestLinkAuthorityRefusesUnknownLevelsAndRestrictedTokens(t *testing.T) {
	saved := selfUser
	t.Cleanup(func() { selfUser = saved })
	medium := identity.User{Kind: "windows", SID: "S-1-5-21-1", Integrity: "medium", IntegrityRID: 0x2000}
	low := identity.User{Kind: "windows", SID: "S-1-5-21-1", Integrity: "low", IntegrityRID: 0x1000}
	unknown := identity.User{Kind: "windows", SID: "S-1-5-21-1", Integrity: "unknown"}
	restricted := medium
	restricted.Restricted = true
	selfUser = func() (identity.User, error) { return unknown, nil }
	if err := sameAuthorityAs(low); err == nil {
		t.Fatal("a provider at an unknown level linked for a low-integrity caller")
	}
	if err := sameAuthorityAs(medium); err == nil {
		t.Fatal("a provider at an unknown level linked")
	}
	selfUser = func() (identity.User, error) { return identity.User{}, errors.New("unreadable") }
	if err := sameAuthorityAs(medium); err == nil {
		t.Fatal("a provider without its own principal linked")
	}
	selfUser = func() (identity.User, error) { return medium, nil }
	for name, caller := range map[string]identity.User{"low": low, "unknown": unknown, "restricted": restricted} {
		if err := sameAuthorityAs(caller); err == nil {
			t.Fatalf("a %s caller linked", name)
		}
	}
	if err := sameAuthorityAs(medium); err != nil {
		t.Fatalf("the provider's own account at its own level: %v", err)
	}
}
