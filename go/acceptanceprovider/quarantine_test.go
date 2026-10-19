package acceptanceprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureLog collects the standard logger's lines for the test's duration.
type logLines struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logLines) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(b)
}

func (l *logLines) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Split(strings.TrimSpace(l.buf.String()), "\n")
}

func captureLog(t *testing.T) *logLines {
	t.Helper()
	lines := &logLines{}
	prior, flags := log.Writer(), log.Flags()
	log.SetOutput(lines)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prior); log.SetFlags(flags) })
	return lines
}

func linesNaming(lines []string, path string) []string {
	var out []string
	for _, line := range lines {
		if strings.Contains(line, path) {
			out = append(out, line)
		}
	}
	return out
}

func rewriteJournal(t *testing.T, path string, edit func(map[string]any)) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	edit(doc)
	data, err = json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

// A corrupt journal moves into requests/unreadable under a timestamped name
// with its bytes intact, newer-schema journals stay in place, and the
// provider starts and replays every good journal.
func TestStartQuarantinesUnreadableJournalAndKeepsNewerSchema(t *testing.T) {
	starts := map[string]func(root string) (*Provider, error){
		"admission": func(root string) (*Provider, error) { return Open(root, "logical-owner") },
		"managed": func(root string) (*Provider, error) {
			return OpenManaged(root, testExecutor{profile: "test-v1", prefix: "private/"})
		},
	}
	for name, start := range starts {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			p, err := start(root)
			if err != nil {
				t.Fatal(err)
			}
			good := map[string]string{}
			for _, key := range []string{"good-1", "good-2", "good-3"} {
				good[key] = accept(t, p.Bind("alice"), submission(p, key)).OperationID
			}
			corruptSub := submission(p, "corrupt")
			corruptOp := accept(t, p.Bind("alice"), corruptSub).OperationID
			newerVersionSub := submission(p, "newer-version")
			newerOp := accept(t, p.Bind("alice"), newerVersionSub).OperationID
			newerFieldSub := submission(p, "newer-field")
			accept(t, p.Bind("alice"), newerFieldSub)
			mismatchSub := submission(p, "mismatch")
			accept(t, p.Bind("alice"), mismatchSub)
			jobsBefore := jobs(t, root)

			corruptPath := p.requestPath("alice", corruptSub.Identity)
			corruptBytes := []byte(`{"Version":`)
			if err := os.WriteFile(corruptPath, corruptBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			newerVersionPath := p.requestPath("alice", newerVersionSub.Identity)
			newerVersionBytes := rewriteJournal(t, newerVersionPath, func(doc map[string]any) { doc["Version"] = 99 })
			newerFieldPath := p.requestPath("alice", newerFieldSub.Identity)
			newerFieldBytes := rewriteJournal(t, newerFieldPath, func(doc map[string]any) { doc["FutureField"] = "from a newer provider" })
			mismatchPath := p.requestPath("alice", mismatchSub.Identity)
			mismatchBytes := rewriteJournal(t, mismatchPath, func(doc map[string]any) { doc["Scope"] = "bob" })

			lines := captureLog(t)
			p, err = start(root)
			if err != nil {
				t.Fatalf("one unreadable journal stopped the start: %v", err)
			}

			for key, id := range good {
				v, err := p.Bind("alice").Reconcile(submission(p, key).Identity)
				if err != nil || v.Receipt == nil || v.Receipt.OperationID != id {
					t.Fatalf("good journal %s lost: %+v %v", key, v, err)
				}
			}
			if jobs(t, root) != jobsBefore {
				t.Fatal("start created or removed job records")
			}

			// Work whose journal was skipped has no subject to decide standing
			// for, and a standing rule holds it. Journaled work still claims.
			p.SetStanding(func(context.Context, AuthenticatedSubject, string) (string, error) { return "", nil })
			store := p.executorStore(context.Background())
			for _, id := range []string{corruptOp, newerOp} {
				if _, err := store.Claim(id, "worker", time.Minute); !errors.Is(err, errStandingUnavailable) {
					t.Fatalf("work without a readable journal claimed: %v", err)
				}
			}
			if _, err := store.Claim(good["good-1"], "worker", time.Minute); err != nil {
				t.Fatalf("journaled work refused: %v", err)
			}

			unreadable := filepath.Join(root, "acceptance", "requests", unreadableFolder)
			for path, want := range map[string][]byte{corruptPath: corruptBytes, mismatchPath: mismatchBytes} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("unreadable journal %s stayed in place: %v", path, err)
				}
				stem := strings.TrimSuffix(filepath.Base(path), ".json")
				moved, err := filepath.Glob(filepath.Join(unreadable, stem+"-*Z.json"))
				if err != nil || len(moved) != 1 {
					t.Fatalf("moved journal for %s: %v %v", path, moved, err)
				}
				got, err := os.ReadFile(moved[0])
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("moved journal changed: %q %v", got, err)
				}
				named := linesNaming(lines.lines(), path)
				if len(named) != 1 || !strings.Contains(named[0], moved[0]) {
					t.Fatalf("want one log line naming %s and %s, got %q", path, moved[0], named)
				}
			}

			for path, want := range map[string][]byte{newerVersionPath: newerVersionBytes, newerFieldPath: newerFieldBytes} {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("newer-schema journal %s moved or changed: %v\n%s", path, err, strings.Join(lines.lines(), "\n"))
				}
				named := linesNaming(lines.lines(), path)
				if len(named) != 1 || !strings.Contains(named[0], "newer schema") {
					t.Fatalf("want one newer-schema log line naming %s, got %q", path, named)
				}
			}

			// A second start reads past the unreadable folder and the
			// newer-schema journals again, and moves nothing more.
			if _, err := start(root); err != nil {
				t.Fatalf("second start: %v", err)
			}
			entries, err := os.ReadDir(unreadable)
			if err != nil || len(entries) != 2 {
				t.Fatalf("unreadable folder after second start: %v %v", entries, err)
			}
			for _, path := range []string{newerVersionPath, newerFieldPath} {
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("newer-schema journal %s gone after second start: %v", path, err)
				}
			}
		})
	}
}

// Work whose journal an earlier start moved aside stays held on every later
// start, while journaled work still claims.
func TestUnjournaledWorkStaysHeldAcrossRestarts(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	lost := accept(t, p.Bind("alice"), submission(p, "lost")).OperationID
	kept := accept(t, p.Bind("alice"), submission(p, "kept")).OperationID
	if err := os.WriteFile(p.requestPath("alice", submission(p, "lost").Identity), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	captureLog(t)
	for start := 1; start <= 2; start++ {
		p = openTest(t, root)
		p.SetStanding(func(context.Context, AuthenticatedSubject, string) (string, error) { return "", nil })
		store := p.executorStore(context.Background())
		if _, err := store.Claim(lost, "worker", time.Minute); !errors.Is(err, errStandingUnavailable) {
			t.Fatalf("start %d: unjournaled work claimed: %v", start, err)
		}
		if start == 2 {
			if _, err := store.Claim(kept, "worker", time.Minute); err != nil {
				t.Fatalf("start %d: journaled work refused: %v", start, err)
			}
		}
	}
}

// An unreadable folder that cannot be listed holds unjournaled work.
func TestUnreadableFolderThatCannotBeListedHoldsWork(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	lost := accept(t, p.Bind("alice"), submission(p, "lost")).OperationID
	path := p.requestPath("alice", submission(p, "lost").Identity)
	// The journal leaves by another route, and a file stands where the
	// unreadable folder belongs: listing it fails with something other than
	// absence.
	if err := os.Rename(path, filepath.Join(root, "journal-elsewhere.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "acceptance", "requests", unreadableFolder), []byte("not a folder"), 0o600); err != nil {
		t.Fatal(err)
	}
	captureLog(t)
	p = openTest(t, root)
	p.SetStanding(func(context.Context, AuthenticatedSubject, string) (string, error) { return "", nil })
	if _, err := p.executorStore(context.Background()).Claim(lost, "worker", time.Minute); !errors.Is(err, errStandingUnavailable) {
		t.Fatalf("unjournaled work claimed: %v", err)
	}
}

// A journal that reads again by the time it would move stays and is indexed.
func TestJournalReadableAgainIsIndexed(t *testing.T) {
	root := t.TempDir()
	p := openTest(t, root)
	s := submission(p, "healed")
	id := accept(t, p.Bind("alice"), s).OperationID
	path := p.requestPath("alice", s.Identity)
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	j, err := p.loadStartJournal(path, []byte(`{`))
	if err != nil || j == nil || j.Receipt == nil || j.Receipt.OperationID != id {
		t.Fatalf("readable journal not indexed: %+v %v", j, err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, good) {
		t.Fatalf("readable journal moved or changed: %v", err)
	}
}
