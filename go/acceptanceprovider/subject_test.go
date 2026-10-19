package acceptanceprovider

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/remote"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

type subjectRemoteFrames struct {
	handler remote.Handler
	peer    remote.Peer
}

func (f subjectRemoteFrames) ExchangeFrame(frame []byte) ([]byte, error) {
	return f.handler(context.Background(), f.peer, frame)
}

type subjectCheckingExecutor struct {
	checkingExecutor
	seen *AuthenticatedSubject
	peer *identity.Peer
}

func (e *subjectCheckingExecutor) CheckSubjectAdmission(binding Binding, _ string, _ []byte, _ []string) (api.AcceptanceOutcome, string) {
	if binding.Subject != nil {
		copy := *binding.Subject
		e.seen = &copy
	}
	e.peer = binding.Peer
	return e.CheckAdmission(binding.Scope, "", nil, nil)
}

func testBinding(t *testing.T) Binding {
	t.Helper()
	path := filepath.Join(t.TempDir(), "caller.exe")
	subject := AuthenticatedSubject{AccountKind: "posix", Account: "1000", Executable: path, Program: path}
	scope, err := LocalSubjectScope(subject.AccountKind, subject.Account, subject.Executable)
	if err != nil {
		t.Fatal(err)
	}
	return Binding{Scope: scope, Subject: &subject, Origin: "local"}
}

func TestJournalRetainsExactReceiverSubjectAcrossRestart(t *testing.T) {
	root := t.TempDir()
	binding := testBinding(t)
	e := &subjectCheckingExecutor{checkingExecutor: newCheckingExecutor()}
	p, err := OpenWithExecutor(root, "owner", e)
	if err != nil {
		t.Fatal(err)
	}
	s := submission(p, "subject")
	receipt := accept(t, p.BindBinding(binding), s)
	if e.seen == nil || *e.seen != *binding.Subject {
		t.Fatalf("admission received %+v", e.seen)
	}
	got, found, err := p.SubjectForOperation(receipt.OperationID, binding.Scope)
	if err != nil || !found || got == nil || *got != *binding.Subject {
		t.Fatalf("current subject %+v %v %v", got, found, err)
	}
	if _, _, err := p.SubjectForOperation(receipt.OperationID, "other-scope"); err == nil {
		t.Fatal("operation ID crossed scope")
	}
	if _, _, err := p.SubjectForOperation(receipt.OperationID, ""); err == nil {
		t.Fatal("public scoped lookup accepted an omitted scope")
	}
	owner, ok, ownerErr := p.executionSubjectForOperation(receipt.OperationID, "")
	if ownerErr != nil || !ok || owner == nil || *owner != *binding.Subject {
		t.Fatal("execution owner did not come from exact journal", owner, ok, ownerErr)
	}
	p, err = OpenWithExecutor(root, "owner", e)
	if err != nil {
		t.Fatal(err)
	}
	got, found, err = p.SubjectForOperation(receipt.OperationID, binding.Scope)
	if err != nil || !found || got == nil || *got != *binding.Subject {
		t.Fatalf("recovered subject %+v %v %v", got, found, err)
	}
	legacy := accept(t, p.Bind(binding.Scope), submission(p, "legacy"))
	got, found, err = p.SubjectForOperation(legacy.OperationID, binding.Scope)
	if err != nil || !found || got != nil {
		t.Fatalf("legacy subject upgraded %+v %v %v", got, found, err)
	}
	if owner, ok, err := p.executionSubjectForOperation(legacy.OperationID, ""); err != nil || !ok || owner != nil {
		t.Fatal("legacy journal became a runtime owner", owner, ok, err)
	}
	if _, err := os.Stat(filepath.Join(root, "acceptance", "owner.json")); err != nil {
		t.Fatal(err)
	}
}

func TestSubjectAdmissionRefusalLeavesNoJournal(t *testing.T) {
	root := t.TempDir()
	binding := testBinding(t)
	e := &subjectCheckingExecutor{checkingExecutor: newCheckingExecutor()}
	p, err := OpenWithExecutor(root, "owner", e)
	if err != nil {
		t.Fatal(err)
	}
	e.set(api.AcceptanceOutcomeInvalid, true)
	result, err := p.BindBinding(binding).Submit(submission(p, "refused"))
	if err != nil || result.Outcome != api.AcceptanceOutcomeInvalid || e.seen == nil {
		t.Fatalf("refusal %+v %v", result, err)
	}
	entries, err := os.ReadDir(p.requests())
	if err != nil || len(entries) != 0 {
		t.Fatalf("refusal left journal: %v %v", entries, err)
	}
}

// TestALocalBindingCarriesItsPeerToAdmission: the native peer a local
// binding was derived from reaches the executor's admission, and a binding of
// any other origin carrying a peer is invalid.
func TestALocalBindingCarriesItsPeerToAdmission(t *testing.T) {
	binding := testBinding(t)
	binding.Peer = &identity.Peer{Platform: "fixture"}
	e := &subjectCheckingExecutor{checkingExecutor: newCheckingExecutor()}
	p, err := OpenWithExecutor(t.TempDir(), "owner", e)
	if err != nil {
		t.Fatal(err)
	}
	accept(t, p.BindBinding(binding), submission(p, "peer"))
	if e.peer != binding.Peer {
		t.Fatalf("admission received peer %v, want the binding's", e.peer)
	}
	key := sha256.Sum256([]byte("trusted remote public key"))
	subject := AuthenticatedSubject{AccountKind: "posix", Account: "1000", Program: "remote:" + hex.EncodeToString(key[:])}
	scope, err := RemoteSubjectScope(subject.AccountKind, subject.Account, subject.Program)
	if err != nil {
		t.Fatal(err)
	}
	remoteBinding := Binding{Scope: scope, Subject: &subject, Origin: "remote", Evidence: hex.EncodeToString(key[:]), Peer: binding.Peer}
	if remoteBinding.valid() == nil {
		t.Fatal("a remote binding carried a native peer")
	}
	remoteBinding.Peer = nil
	if err := remoteBinding.valid(); err != nil {
		t.Fatalf("the remote binding without a peer: %v", err)
	}
}

func remoteSubjectPeer(spki string) remote.Peer {
	certificate := &x509.Certificate{RawSubjectPublicKeyInfo: []byte(spki), Subject: pkix.Name{CommonName: "same display name"}, DNSNames: []string{"claimed-account"}}
	return remote.Peer{Certificate: certificate, Key: sha256.Sum256(certificate.RawSubjectPublicKeyInfo)}
}

func TestRemoteSubjectComesFromLivePinnedKey(t *testing.T) {
	root := t.TempDir()
	e := &subjectCheckingExecutor{checkingExecutor: newCheckingExecutor()}
	p, err := OpenWithExecutor(root, "owner", e)
	if err != nil {
		t.Fatal(err)
	}
	peer := remoteSubjectPeer("trusted remote public key")
	other := remoteSubjectPeer("another key with the same display claims")
	revoked := false
	admit := func(candidate remote.Peer) error {
		if revoked || candidate.Key != peer.Key {
			return errors.New("unmapped or revoked key")
		}
		return nil
	}
	config := RemoteConfig{AccountKind: "posix", Account: "1000", Admit: admit}
	policy := func(context.Context, remote.Peer, string, string) error { return nil }
	client := api.NewRecoverableAcceptanceClient(subjectRemoteFrames{handler: p.RemoteHandler(config, policy), peer: peer})
	s := submission(p, "mapped")
	receipt := accept(t, client, s)
	program := "remote:" + hex.EncodeToString(peer.Key[:])
	scope, err := RemoteSubjectScope("posix", "1000", program)
	if err != nil {
		t.Fatal(err)
	}
	want := AuthenticatedSubject{AccountKind: "posix", Account: "1000", Program: program}
	got, found, err := p.SubjectForOperation(receipt.OperationID, scope)
	if err != nil || !found || got == nil || *got != want || e.peer != nil || e.seen == nil || *e.seen != want {
		t.Fatalf("mapped subject %+v %v %v executor %+v peer%v", got, found, err, e.seen, e.peer)
	}
	stranger := api.NewRecoverableAcceptanceClient(subjectRemoteFrames{handler: p.RemoteHandler(config, policy), peer: other})
	if v, err := stranger.Submit(s); err != nil || v.Outcome != api.AcceptanceOutcomeForbidden {
		t.Fatalf("unmapped certificate %+v %v", v, err)
	}
	revoked = true
	if v, err := client.Reconcile(s.Identity); err != nil || v.Outcome != api.AcceptanceOutcomeForbidden {
		t.Fatalf("live revocation %+v %v", v, err)
	}
	revoked = false
	p, err = OpenWithExecutor(root, "owner", e)
	if err != nil {
		t.Fatal(err)
	}
	client = api.NewRecoverableAcceptanceClient(subjectRemoteFrames{handler: p.RemoteHandler(config, policy), peer: peer})
	if v, err := client.Reconcile(s.Identity); err != nil || v.Receipt == nil || !reflect.DeepEqual(*v.Receipt, receipt) {
		t.Fatalf("restart/revocation changed ownership %+v %v", v, err)
	}
	config.Account = "1001"
	isolated := api.NewRecoverableAcceptanceClient(subjectRemoteFrames{handler: p.RemoteHandler(config, policy), peer: peer})
	if v, err := isolated.Reconcile(s.Identity); err != nil || v.Outcome != api.AcceptanceOutcomeDefinitelyNotAccepted || v.Receipt != nil {
		t.Fatalf("account crossed ownership %+v %v", v, err)
	}
}

func TestRemoteBindingRequiresCanonicalScopeAndEvidence(t *testing.T) {
	peer := remoteSubjectPeer("binding key")
	program := "remote:" + hex.EncodeToString(peer.Key[:])
	scope, err := RemoteSubjectScope("posix", "1000", program)
	if err != nil {
		t.Fatal(err)
	}
	base := Binding{Scope: scope, Subject: &AuthenticatedSubject{AccountKind: "posix", Account: "1000", Program: program}, Origin: "remote", Evidence: hex.EncodeToString(peer.Key[:])}
	for _, change := range []func(*Binding){
		func(b *Binding) { b.Scope = "arbitrary" },
		func(b *Binding) { b.Evidence = strings.ToUpper(b.Evidence) },
		func(b *Binding) { b.Evidence = strings.Repeat("ab", 32) },
		func(b *Binding) { b.Subject.Program = "/services/receiving-runtime" },
		func(b *Binding) { b.Subject.Executable = filepath.Join(t.TempDir(), "fake.exe") },
		func(b *Binding) { b.Peer = &identity.Peer{Platform: "fixture"} },
	} {
		b := base
		subject := *base.Subject
		b.Subject = &subject
		change(&b)
		if err := b.valid(); err == nil {
			t.Fatalf("invalid remote binding accepted %+v", b)
		}
	}
	local := testBinding(t)
	local.Subject.Program = program
	if err := local.valid(); err == nil {
		t.Fatal("local binding accepted a remote program")
	}
}

func TestRemoteHandlerRequiresEvidenceAndSeparatesPinnedKeys(t *testing.T) {
	p := openTest(t, t.TempDir())
	first, second := remoteSubjectPeer("first key"), remoteSubjectPeer("second key")
	admit := func(peer remote.Peer) error {
		if peer.Key == first.Key || peer.Key == second.Key {
			return nil
		}
		return remote.ErrTrust
	}
	config := RemoteConfig{AccountKind: "posix", Account: "1000", Admit: admit}
	policy := func(context.Context, remote.Peer, string, string) error { return nil }
	client := func(peer remote.Peer, config RemoteConfig) *api.RecoverableAcceptanceClient {
		return api.NewRecoverableAcceptanceClient(subjectRemoteFrames{handler: p.RemoteHandler(config, policy), peer: peer})
	}
	s := submission(p, "same-key")
	receipt := accept(t, client(first, config), s)
	otherOperations := api.NewOperationControlClient(subjectRemoteFrames{handler: p.RemoteHandler(config, policy), peer: second})
	if v, err := otherOperations.ObserveWork(s.Identity); err != nil || v.Outcome != api.ObservationOutcomeUnknown || v.Snapshot != nil {
		t.Fatalf("same display name crossed key scope %+v %v", v, err)
	}
	other := accept(t, client(second, config), s)
	if other.OperationID == receipt.OperationID {
		t.Fatal("different keys shared operation")
	}
	bad := second
	bad.Key = first.Key
	for _, peer := range []remote.Peer{{Key: first.Key}, bad, {}} {
		if v, err := client(peer, config).Submit(submission(p, "missing-evidence")); err != nil || v.Outcome != api.AcceptanceOutcomeForbidden {
			t.Fatalf("invalid evidence %+v %v", v, err)
		}
	}
	for _, badConfig := range []RemoteConfig{{AccountKind: "posix", Account: "1000"}, {AccountKind: "posix", Admit: admit}, {AccountKind: "remote", Account: "1000", Admit: admit}} {
		if v, err := client(first, badConfig).Submit(submission(p, "bad-host-config")); err != nil || v.Outcome != api.AcceptanceOutcomeForbidden {
			t.Fatalf("invalid host config %+v %v", v, err)
		}
	}
}

func TestUnsafeOldRemoteJournalIsRefusedAndPreserved(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	peer := remoteSubjectPeer("journal key")
	program := "remote:" + hex.EncodeToString(peer.Key[:])
	scope, err := RemoteSubjectScope("posix", "1000", program)
	if err != nil {
		t.Fatal(err)
	}
	b := Binding{Scope: scope, Subject: &AuthenticatedSubject{AccountKind: "posix", Account: "1000", Program: program}, Origin: "remote", Evidence: hex.EncodeToString(peer.Key[:])}
	s := submission(p, "old-arbitrary-subject")
	accept(t, p.BindBinding(b), s)
	path := p.requestPath(scope, s.Identity)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var j journal
	if err := json.Unmarshal(data, &j); err != nil {
		t.Fatal(err)
	}
	j.Subject.Program = "/old/arbitrary/remote-program"
	data, err = json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, "owner"); err == nil {
		t.Fatal("unsafe old remote journal reopened")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(data) {
		t.Fatal("refusal changed retained journal")
	}
}

func TestClaimedBoundJournalNeverFallsBackToLegacyScope(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	binding := testBinding(t)
	s := submission(p, "corrupt-subject")
	receipt := accept(t, p.BindBinding(binding), s)
	path := p.requestPath(binding.Scope, s.Identity)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var j journal
	if err := json.Unmarshal(data, &j); err != nil {
		t.Fatal(err)
	}
	j.Subject = nil // SubjectOrigin still says this operation was bound.
	data, err = json.Marshal(&j)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := p.SubjectForOperation(receipt.OperationID, binding.Scope); !found || err == nil {
		t.Fatalf("corrupt subject used fallback: found=%v err=%v", found, err)
	}
	if _, err := Open(root, "logical-owner"); err == nil {
		t.Fatal("corrupt bound journal reopened as legacy")
	}
}
