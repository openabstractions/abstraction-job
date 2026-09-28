package acceptanceprovider

import (
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A CAS replacement can briefly deny opening a job record on Windows. An
// inventory read waits for that transient state instead of reporting an
// unavailable inventory while the accepted work remains present.
func TestInventoryWaitsForTransientJobRecordSharingViolation(t *testing.T) {
	p := openTest(t, t.TempDir())
	defer p.CloseInventory()
	receipt := accept(t, p.Bind("own"), submission(p, "one"))
	path := filepath.Join(p.store.Root(), "jobs", receipt.OperationID+".json")
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = syscall.CloseHandle(h) }) }
	defer release()
	if err := regularFile(path); err != nil {
		t.Fatalf("record metadata while held: %v", err)
	}
	type answer struct {
		outcome string
		count   int
		err     error
	}
	done := make(chan answer, 1)
	go func() {
		page, err := p.BindInventory("own").ListWork("", 1)
		done <- answer{page.Outcome.String(), len(page.Snapshots), err}
	}()
	select {
	case got := <-done:
		t.Fatalf("inventory returned during transient sharing violation: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case got := <-done:
		if got.err != nil || got.outcome != "page" || got.count != 1 {
			t.Fatalf("inventory after sharing violation: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("inventory did not finish after the record was released")
	}
}
