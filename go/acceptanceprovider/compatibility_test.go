package acceptanceprovider

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func storageTree(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		name, _ := filepath.Rel(root, path)
		if entry.IsDir() {
			result[name] = "directory"
			return nil
		}
		data, err := os.ReadFile(path)
		result[name] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCheckManagedFreshAndExistingAreReadOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	e := testExecutor{profile: "compatible-v1"}
	if err := CheckManaged(root, e); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preflight created fresh root", err)
	}
	p, err := OpenManaged(root, e)
	if err != nil {
		t.Fatal(err)
	}
	before := storageTree(t, root)
	if err = CheckManaged(root, e); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, storageTree(t, root)) {
		t.Fatal("preflight modified existing store")
	}
	if _, err = OpenManaged(root, e); err != nil {
		t.Fatal(err)
	}
	if p.LogicalOwner() == "" {
		t.Fatal("missing owner")
	}
}

func TestUnsupportedWriterRefusesBeforeMetadataWrites(t *testing.T) {
	for _, mode := range []string{"future-version", "wrong-profile", "admission-only-rollback", "unknown-field", "duplicate-version"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "acceptance"), 0700); err != nil {
				t.Fatal(err)
			}
			cfg := configuration{Version: 2, Owner: "retained-owner", Epoch: "retained-epoch", RetentionMs: MinimumRetentionMs, Execution: "compatible-v1"}
			var e Executor = testExecutor{profile: "compatible-v1"}
			if mode == "future-version" {
				cfg.Version = 99
			}
			if mode == "wrong-profile" {
				e = testExecutor{profile: "other-profile"}
			}
			if mode == "admission-only-rollback" {
				e = nil
			}
			raw, _ := json.Marshal(cfg)
			if mode == "unknown-field" {
				raw = []byte(strings.TrimSuffix(string(raw), "}") + `,"FutureMeaning":true}`)
			}
			if mode == "duplicate-version" {
				raw = []byte(strings.Replace(string(raw), `"Version":2`, `"Version":99,"Version":2`, 1))
			}
			if err := os.WriteFile(filepath.Join(root, "acceptance", "owner.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			before := storageTree(t, root)
			if err := CheckManaged(root, e); err == nil {
				t.Fatal("unsupported preflight succeeded")
			}
			if _, err := OpenManaged(root, e); err == nil {
				t.Fatal("unsupported writer opened")
			}
			if !reflect.DeepEqual(before, storageTree(t, root)) {
				t.Fatal("unsupported writer changed files or created lock/directories")
			}
		})
	}
}

func TestCheckManagedRejectsUnownedHistoryWithoutAdoption(t *testing.T) {
	for _, directory := range []string{"jobs", filepath.Join("acceptance", "requests")} {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, directory), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, directory, "old.json"), []byte("retained"), 0600); err != nil {
			t.Fatal(err)
		}
		before := storageTree(t, root)
		if err := CheckManaged(root, nil); !errors.Is(err, ErrIncompatibleStorage) {
			t.Fatal(err)
		}
		if _, err := OpenManaged(root, nil); !errors.Is(err, ErrIncompatibleStorage) {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, storageTree(t, root)) {
			t.Fatal("unowned store changed")
		}
	}
}

func TestCheckManagedIsCompatibilityNotJournalRecovery(t *testing.T) {
	root := t.TempDir()
	if _, err := OpenManaged(root, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "acceptance", "requests", "retained.json")
	if err := os.WriteFile(path, []byte("incomplete journal"), 0600); err != nil {
		t.Fatal(err)
	}
	before := storageTree(t, root)
	if err := CheckManaged(root, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, storageTree(t, root)) {
		t.Fatal("preflight attempted recovery")
	}
	if _, err := OpenManaged(root, nil); err == nil {
		t.Fatal("Open ignored invalid recovery journal")
	}
}

func TestOwnerMetadataUnicodePreservedOrRefused(t *testing.T) {
	for _, value := range []struct {
		raw   string
		valid bool
	}{
		{`"\ud800"`, false}, {`"\udc00"`, false}, {`"` + string([]byte{0xff}) + `"`, false},
		{`"\ud83d\ude00"`, true}, {`"�"`, true}, {`"\\ud800"`, true},
	} {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "acceptance"), 0700); err != nil {
			t.Fatal(err)
		}
		raw := []byte(`{"Version":1,"Owner":` + value.raw + `,"Epoch":"retained","RetentionMs":86400000}`)
		if err := os.WriteFile(filepath.Join(root, "acceptance", "owner.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		before := storageTree(t, root)
		err := CheckManaged(root, nil)
		if (err == nil) != value.valid {
			t.Fatalf("%s: %v", value.raw, err)
		}
		if !value.valid {
			if _, err := OpenManaged(root, nil); err == nil {
				t.Fatal("invalid Unicode writer opened")
			}
		}
		if !reflect.DeepEqual(before, storageTree(t, root)) {
			t.Fatal("Unicode preflight changed original bytes")
		}
	}
}
