package acceptanceprovider

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	job "github.com/openabstractions/abstraction-job/go"
)

func ownerHeader(t *testing.T, root string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "acceptance", "owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestStorageFeatureMarksRetryBeforeJournal(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	c := p.Bind("alice")
	original := submission(p, "feature-key")
	first := accept(t, c, original)
	before := ownerHeader(t, root)
	if bytes.Contains(before, []byte("Features")) {
		t.Fatalf("original request recorded a storage feature: %s", before)
	}
	// An ineligible attempt writes neither a journal nor a feature.
	if got := outcomeOf(t)(c.Submit(retry(original, 1))); got != "invalid" {
		t.Fatalf("ineligible retry: %s", got)
	}
	if !bytes.Equal(before, ownerHeader(t, root)) {
		t.Fatal("ineligible retry changed the owner header")
	}
	endOperation(t, p, first.OperationId, job.StateFailed)

	// A crash between the marker and the journal leaves the marker alone.
	p.fault = func(point string) error {
		if point == "after-storage-feature" {
			return errors.New("interrupted")
		}
		return nil
	}
	if got := outcomeOf(t)(c.Submit(retry(original, 1))); got != "unknown" {
		t.Fatalf("interrupted retry: %s", got)
	}
	if !bytes.Contains(ownerHeader(t, root), []byte(StorageFeatureAttempts)) {
		t.Fatalf("marker absent after the interrupted retry: %s", ownerHeader(t, root))
	}
	if _, err := os.Stat(p.requestPath("alice", retry(original, 1).Identity)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal written before its marker was confirmed: %v", err)
	}
	p.fault = nil
	accept(t, c, retry(original, 1))
	header := ownerHeader(t, root)
	if strings.Count(string(header), StorageFeatureAttempts) != 1 {
		t.Fatalf("feature recorded %d times: %s", strings.Count(string(header), StorageFeatureAttempts), header)
	}

	// The current provider and its preflight accept the marked store.
	if err := CheckManaged(root, nil); err != nil {
		t.Fatalf("current preflight: %v", err)
	}
	reopened := openTest(t, root)
	if v, err := reopened.Bind("alice").Reconcile(retry(original, 1).Identity); err != nil || v.Outcome != "accepted" {
		t.Fatalf("current provider after marker: %+v %v", v, err)
	}

	// An older validator refuses by feature name, before any journal is read.
	_, err := validateConfigurationFeatures(header, "", 1, "", true, nil)
	if !errors.Is(err, ErrIncompatibleStorage) || !strings.Contains(err.Error(), StorageFeatureAttempts) {
		t.Fatalf("older validator: %v", err)
	}
	_, err = validateConfigurationFeatures(header, "", 1, "", true, []string{StorageFeatureResultLost})
	if !errors.Is(err, ErrIncompatibleStorage) || !strings.Contains(err.Error(), StorageFeatureAttempts) {
		t.Fatalf("validator missing one feature: %v", err)
	}
}

func TestStorageFeatureMarksResultLoss(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	original := submission(p, "lost-feature")
	accept(t, p.Bind("alice"), original)
	if err := p.recordResultLost("alice", original.Identity); err != nil {
		t.Fatal(err)
	}
	header := ownerHeader(t, root)
	if !bytes.Contains(header, []byte(StorageFeatureResultLost)) || bytes.Contains(header, []byte(StorageFeatureAttempts)) {
		t.Fatalf("owner header after a recorded loss: %s", header)
	}
	if !p.resultLost("alice", original.Identity) {
		t.Fatal("loss not recorded")
	}
	if _, err := validateConfigurationFeatures(header, "", 1, "", true, []string{StorageFeatureAttempts}); err == nil || !strings.Contains(err.Error(), StorageFeatureResultLost) {
		t.Fatalf("older validator: %v", err)
	}
}

func TestUnsupportedStorageFeatureRefusesBeforeWrites(t *testing.T) {
	for name, header := range map[string]string{
		"unknown":   `{"Version":1,"Owner":"logical-owner","Epoch":"e","RetentionMs":86400000,"Features":["abstraction.job/journal-future@9"]}`,
		"duplicate": `{"Version":1,"Owner":"logical-owner","Epoch":"e","RetentionMs":86400000,"Features":["abstraction.job/journal-attempts@1","abstraction.job/journal-attempts@1"]}`,
		"empty":     `{"Version":1,"Owner":"logical-owner","Epoch":"e","RetentionMs":86400000,"Features":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "acceptance", "owner.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(header), 0o600); err != nil {
				t.Fatal(err)
			}
			entries := func() []string {
				var all []string
				filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
					all = append(all, p)
					return nil
				})
				return all
			}
			before := entries()
			if err := CheckManaged(root, nil); !errors.Is(err, ErrIncompatibleStorage) {
				t.Fatalf("preflight: %v", err)
			}
			if _, err := Open(root, "logical-owner"); !errors.Is(err, ErrIncompatibleStorage) {
				t.Fatalf("open: %v", err)
			}
			if name == "unknown" {
				if _, err := Open(root, "logical-owner"); !strings.Contains(err.Error(), "abstraction.job/journal-future@9") {
					t.Fatalf("refusal does not name the feature: %v", err)
				}
			}
			if after := entries(); strings.Join(after, "\n") != strings.Join(before, "\n") {
				t.Fatalf("refusal wrote files:\n%v\n%v", before, after)
			}
		})
	}
}
