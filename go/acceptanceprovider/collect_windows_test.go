package acceptanceprovider

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	job "github.com/openabstractions/abstraction-job/go"
)

func TestCollectTmpKeepsJunctionParent(t *testing.T) {
	f := collectionFixture(t, job.StateComplete)
	outside := t.TempDir()
	kept := filepath.Join(outside, f.receipt.OperationID)
	if err := os.WriteFile(kept, []byte("caller fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(f.result)
	if err := os.Rename(parent, parent+"-kept"); err != nil {
		t.Fatal(err)
	}
	if body, err := exec.Command("cmd", "/c", "mklink", "/J", parent, outside).CombinedOutput(); err != nil {
		t.Skip("junction fixture unavailable", string(body), err)
	}
	defer os.Remove(parent)
	if removed, err := f.p.CollectTmp(context.Background(), collectionTime.Add(100*24*time.Hour), time.Hour); err == nil || len(removed) != 0 {
		t.Fatal("junction admitted", removed, err)
	}
	requireKept(t, kept)
}
