package acceptanceprovider

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Hosts that start together on a fresh store race to create the lock file.
// Each must learn only who owns the store: on darwin a plain O_CREAT loser
// could fail with ENOENT (golang/go#81246).
func TestAcquireHostCreateRaceReportsOnlyOwnership(t *testing.T) {
	const rounds, hosts = 50, 32
	for round := 0; round < rounds; round++ {
		root := t.TempDir()
		start := make(chan struct{})
		guards := make([]io.Closer, hosts)
		errs := make([]error, hosts)
		var wg sync.WaitGroup
		for i := 0; i < hosts; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				guards[i], errs[i] = AcquireHost(root)
			}(i)
		}
		close(start)
		wg.Wait()
		owners := 0
		for i := 0; i < hosts; i++ {
			if errs[i] == nil {
				owners++
				guards[i].Close()
			} else if !errors.Is(errs[i], ErrHostActive) {
				t.Errorf("round %d host %d: %v", round, i, errs[i])
			}
		}
		if owners != 1 {
			t.Fatalf("round %d: %d owners", round, owners)
		}
	}
}

func TestHostGuardProcessFixture(t *testing.T) {
	root := os.Getenv("OA_TEST_HOST_GUARD_ROOT")
	if root == "" {
		return
	}
	guard, err := AcquireHost(root)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	fmt.Println("guard-ready")
	for {
		time.Sleep(time.Hour)
	}
}

func TestHostGuardProcessDeathReleasesPersistentLock(t *testing.T) {
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
	if guard, err := AcquireHost(root); !errors.Is(err, ErrHostActive) {
		if guard != nil {
			guard.Close()
		}
		t.Fatalf("live child guard: %v", err)
	}
	path := filepath.Join(root, "acceptance", "host.lock")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	waited = true
	if err == nil {
		t.Fatal("forced child unexpectedly exited successfully")
	}
	guard, err := AcquireHost(root)
	if err != nil {
		t.Fatal("dead child retained guard", err)
	}
	defer guard.Close()
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("guard acquisition replaced persistent lock file")
	}
}

func TestHostGuardRootAliasAndEscapingLock(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	guard, err := AcquireHost(root)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	alias := filepath.Join(base, "alias")
	if err = os.Symlink(root, alias); err != nil {
		t.Skipf("OS refused temporary symlink fixture: %v", err)
	}
	if other, err := AcquireHost(alias); !errors.Is(err, ErrHostActive) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("root alias bypassed guard: %v", err)
	}
	escapeRoot := filepath.Join(base, "escape")
	if err = os.MkdirAll(filepath.Join(escapeRoot, "acceptance"), 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside.lock")
	if err = os.WriteFile(outside, []byte("retained"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(escapeRoot, "acceptance", "host.lock")); err != nil {
		t.Fatal(err)
	}
	if other, err := AcquireHost(escapeRoot); err == nil {
		other.Close()
		t.Fatal("accepted escaping host lock")
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "retained" {
		t.Fatal("escape refusal changed outside file", err)
	}
}
