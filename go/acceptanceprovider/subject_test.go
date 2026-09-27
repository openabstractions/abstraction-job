package acceptanceprovider

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

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
}

func (e *subjectCheckingExecutor) CheckSubjectAdmission(binding Binding, _ string, _ []byte, _ []string) (api.AcceptanceOutcome, string) {
	if binding.Subject != nil {
		copy := *binding.Subject
		e.seen = &copy
	}
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

func TestRemoteSubjectComesFromCertificateKeyMapping(t *testing.T) {
	p := openTest(t, t.TempDir())
	key := sha256.Sum256([]byte("trusted remote public key"))
	subject := AuthenticatedSubject{AccountKind: "posix", Account: "1000", Program: "/services/receiving-runtime"}
	handler := p.RemoteBindingHandler(func(_ context.Context, peer remote.Peer) (Binding, error) {
		if peer.Key != key {
			return Binding{}, errors.New("unmapped key")
		}
		return Binding{Scope: "mapped-remote-peer", Subject: &subject, Origin: "remote"}, nil
	}, nil)
	client := api.NewRecoverableAcceptanceClient(subjectRemoteFrames{handler: handler, peer: remote.Peer{Key: key}})
	receipt := accept(t, client, submission(p, "mapped"))
	got, found, err := p.SubjectForOperation(receipt.OperationID, "mapped-remote-peer")
	if err != nil || !found || got == nil || *got != subject {
		t.Fatalf("mapped subject %+v %v %v", got, found, err)
	}
	stranger := api.NewRecoverableAcceptanceClient(subjectRemoteFrames{handler: handler, peer: remote.Peer{Key: sha256.Sum256([]byte("stranger"))}})
	if v, err := stranger.Submit(submission(p, "unmapped")); err != nil || v.Outcome != api.AcceptanceOutcomeForbidden {
		t.Fatalf("unmapped certificate %+v %v", v, err)
	}
	forgedLocal := p.RemoteBindingHandler(func(context.Context, remote.Peer) (Binding, error) {
		return Binding{Scope: "mapped-remote-peer", Subject: &subject, Origin: "local"}, nil
	}, nil)
	forged := api.NewRecoverableAcceptanceClient(subjectRemoteFrames{handler: forgedLocal, peer: remote.Peer{Key: key}})
	if v, err := forged.Submit(submission(p, "forged-local")); err != nil || v.Outcome != api.AcceptanceOutcomeForbidden {
		t.Fatalf("remote claimed local proof %+v %v", v, err)
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
