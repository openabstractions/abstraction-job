package acceptanceprovider

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// tree records every path under root, and each file's size and modification
// time. NTFS updates a directory's time lazily after an entry is created in it,
// so directories are recorded by mode alone.
func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			got[rel] = info.Mode().String()
			return nil
		}
		got[rel] = fmt.Sprint(info.Mode(), info.ModTime(), info.Size())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func probe(t *testing.T, root string) bool {
	t.Helper()
	active, err := HostActive(root)
	if err != nil {
		t.Fatal(err)
	}
	return active
}

func TestHostActiveMissingLockIsNotHeldAndCreatesNothing(t *testing.T) {
	base := t.TempDir()
	if probe(t, filepath.Join(base, "absent")) {
		t.Fatal("missing root reported held")
	}
	root := filepath.Join(base, "root")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if probe(t, root) {
		t.Fatal("root without acceptance reported held")
	}
	if err := os.Mkdir(filepath.Join(root, "acceptance"), 0700); err != nil {
		t.Fatal(err)
	}
	before := tree(t, base)
	if probe(t, root) {
		t.Fatal("missing lock file reported held")
	}
	if after := tree(t, base); !reflect.DeepEqual(before, after) {
		t.Fatalf("probe changed the store:\nbefore %v\nafter  %v", before, after)
	}
	if _, err := os.Stat(filepath.Join(base, "absent")); !os.IsNotExist(err) {
		t.Fatalf("probe created a missing root: %v", err)
	}
}

func TestHostActiveHeldAndReleasedInProcess(t *testing.T) {
	root := t.TempDir()
	guard, err := AcquireHost(root)
	if err != nil {
		t.Fatal(err)
	}
	before := tree(t, root)
	if !probe(t, root) {
		guard.Close()
		t.Fatal("held store reported free")
	}
	if err = guard.Close(); err != nil {
		t.Fatal(err)
	}
	if probe(t, root) {
		t.Fatal("released store reported held")
	}
	// The probe let go: a host can still take the store.
	again, err := AcquireHost(root)
	if err != nil {
		t.Fatal("probe left the guard taken:", err)
	}
	again.Close()
	if !reflect.DeepEqual(before, tree(t, root)) {
		t.Fatal("probe changed the store")
	}
}

func TestHostActiveSeesAnotherProcessAndItsDeath(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHostGuardProcessFixture$")
	cmd.Env = append(os.Environ(), "OA_TEST_HOST_GUARD_ROOT="+root)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if line != "guard-ready\n" {
			t.Fatalf("child readiness %q", line)
		}
	case <-ctx.Done():
		t.Fatal("child did not acquire guard")
	}
	if !probe(t, root) {
		t.Fatal("store held by another process reported free")
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	waited = true
	for deadline := time.Now().Add(10 * time.Second); probe(t, root); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("dead host still reported held")
		}
	}
}

func TestHostActiveRefusesADirectoryLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "acceptance", "host.lock"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := HostActive(dir); err == nil {
		t.Fatal("accepted a directory as the host lock")
	}
}

func TestHostActiveRefusesAnEscapingLock(t *testing.T) {
	base := t.TempDir()
	escape := filepath.Join(base, "escape")
	if err := os.MkdirAll(filepath.Join(escape, "acceptance"), 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside.lock")
	if err := os.WriteFile(outside, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(escape, "acceptance", "host.lock")); err != nil {
		t.Skipf("OS refused temporary symlink fixture: %v", err)
	}
	if _, err := HostActive(escape); err == nil {
		t.Fatal("followed a host lock outside the store")
	}
}
