//go:build windows

package acceptanceprovider

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/openabstractions/abstraction-identity/fileid"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	"golang.org/x/sys/windows"
)

func otherVolumeError() error { return windows.ERROR_NOT_SAME_DEVICE }

func noHardLinksError() error { return windows.ERROR_INVALID_FUNCTION }

// JOB-A17: while LinkResult holds the destination's parent, that folder
// cannot be renamed away, so the resolved name is the one linked; a link
// made inside it still works, and release lets the folder go.
func TestHeldParentCannotBeReplacedWhileLinking(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "downloads")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	held, err := holdFinalPath(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parent, parent+"-moved"); err == nil {
		held.release()
		t.Fatal("a held parent was renamed")
	}
	source := filepath.Join(t.TempDir(), "result")
	if err := os.WriteFile(source, []byte("bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := held.link(source, "model"); err != nil {
		held.release()
		t.Fatalf("a link inside the held parent: %v", err)
	}
	held.release()
	if err := os.Rename(parent, parent+"-moved"); err != nil {
		t.Fatalf("a released parent: %v", err)
	}
}

// The pathname is an admission/display locator. A controlled stale-locator
// seam must never redirect the effect away from the directory held at admission.
func TestLinkResultHeldParentIgnoresStaleLocator(t *testing.T) {
	f := newLinkFixture(t, true)
	parent, decoy := t.TempDir(), t.TempDir()
	saved := holdFinal
	t.Cleanup(func() { holdFinal = saved })
	holdFinal = func(path string) (*heldLinkParent, error) {
		held, err := saved(path)
		if err == nil {
			held.path = decoy
		}
		return held, err
	}
	r := f.link(t, f.local, filepath.Join(parent, "model"))
	if _, err := os.Lstat(filepath.Join(decoy, "model")); !os.IsNotExist(err) {
		t.Fatalf("effect followed stale locator: %+v %v", r, err)
	}
	if r.Outcome != api.LinkOutcomeLinked {
		t.Fatalf("held parent did not receive result: %+v", r)
	}
	requireSameLinkedFile(t, f.result, filepath.Join(parent, "model"))
}

func TestLinkResultHeldParentInsideRootRefusesOutsideLocator(t *testing.T) {
	f := newLinkFixture(t, true)
	inside, outside := filepath.Join(f.p.root, "results"), t.TempDir()
	saved := holdFinal
	t.Cleanup(func() { holdFinal = saved })
	holdFinal = func(string) (*heldLinkParent, error) {
		held, err := saved(inside)
		if err == nil {
			held.path = outside
		}
		return held, err
	}
	if r := f.link(t, f.local, filepath.Join(inside, "escaped")); r.Outcome != api.LinkOutcomeInvalid {
		t.Fatalf("inside held object admitted by outside locator: %+v", r)
	}
	if _, err := os.Lstat(filepath.Join(inside, "escaped")); !os.IsNotExist(err) {
		t.Fatalf("provider layout changed: %v", err)
	}
}

// Existence is decided by the native creation, including a name gained after
// admission. Every refused leaf leaves the source and other names untouched.
func TestLinkResultHeldParentAtomicCollisionAndUnsafeLeaf(t *testing.T) {
	parent := t.TempDir()
	held, err := holdFinalPath(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer held.release()
	source := filepath.Join(t.TempDir(), "result")
	if err := os.WriteFile(source, []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(parent, "present")
	if err := os.WriteFile(dest, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := held.link(source, "present"); !os.IsExist(err) {
		t.Fatalf("collision: %v", err)
	}
	if data, err := os.ReadFile(dest); err != nil || string(data) != "original" {
		t.Fatalf("existing bytes changed: %q %v", data, err)
	}
	for _, leaf := range []string{"", ".", "..", "../escaped", `sub\escaped`, "model:stream", "NUL"} {
		if err := held.link(source, leaf); err == nil {
			t.Fatalf("unsafe leaf %q linked", leaf)
		}
	}
	if err := held.link(source, "model-\u03bb"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source retention: %v", err)
	}
}

func TestLinkResultHeldParentOrdinaryAndExtendedLeaves(t *testing.T) {
	f := newLinkFixture(t, true)
	parent := t.TempDir()
	ordinary := filepath.Join(parent, "ordinary.")
	if r := f.link(t, f.local, ordinary); r.Outcome != api.LinkOutcomeLinked {
		t.Fatalf("ordinary leaf: %+v", r)
	}
	requireSameLinkedFile(t, f.result, filepath.Join(parent, "ordinary"))
	extended := `\\?\` + filepath.Join(parent, "verbatim. ")
	if r := f.link(t, f.local, extended); r.Outcome != api.LinkOutcomeLinked {
		t.Fatalf("extended leaf: %+v", r)
	}
	requireSameLinkedFile(t, f.result, extended)
	if _, err := os.Lstat(filepath.Join(parent, "verbatim")); !os.IsNotExist(err) {
		t.Fatalf("extended leaf was normalized: %v", err)
	}
}

// Windows currently refuses this ancestor rename while the descendant is held.
// A filesystem which admits it must still place the effect in the held folder.
func TestLinkResultHeldParentAncestorRename(t *testing.T) {
	base := t.TempDir()
	ancestor := filepath.Join(base, "ancestor")
	parent := filepath.Join(ancestor, "downloads")
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	held, err := holdFinalPath(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer held.release()
	moved := ancestor + "-moved"
	err = os.Rename(ancestor, moved)
	actualParent := parent
	if err == nil {
		actualParent = filepath.Join(moved, "downloads")
		if err := os.MkdirAll(parent, 0700); err != nil {
			t.Fatal(err)
		}
	} else if !os.IsPermission(err) {
		t.Fatalf("ancestor rename: %v", err)
	}
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := held.link(source, "model"); err != nil {
		t.Fatal(err)
	}
	requireSameLinkedFile(t, source, filepath.Join(actualParent, "model"))
	if actualParent != parent {
		if _, err := os.Lstat(filepath.Join(parent, "model")); !os.IsNotExist(err) {
			t.Fatalf("substituted ancestor received link: %v", err)
		}
	}
}

func TestLinkResultHeldParentSourceAndNativeFailureRelease(t *testing.T) {
	parent := t.TempDir()
	held, err := holdFinalPath(parent)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "result")
	if err := os.WriteFile(source, []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := held.link(source+"-absent", "absent"); !os.IsNotExist(err) {
		t.Fatalf("missing source: %v", err)
	}
	if err := held.link(filepath.Dir(source), "directory"); err == nil {
		t.Fatal("directory source linked")
	}
	held.release()
	if err := held.link(source, "closed-parent"); err == nil {
		t.Fatal("closed parent linked")
	}
	// The failed native call closes its source handle. Reopening exclusively
	// for data access would be refused by a leaked source handle's share mode.
	name, err := windows.UTF16PtrFromString(source)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.DELETE, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatalf("failed link retained source handle: %v", err)
	}
	if err := windows.CloseHandle(h); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed links created names: %v %v", entries, err)
	}
}

func TestLinkResultHeldParentMovedSnapshotRefused(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "parent")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	held, err := fileid.Open(context.Background(), fileid.Self, parent, fileid.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := os.Rename(parent, parent+"-moved"); err != nil {
		t.Fatalf("owned pre-pin move: %v", err)
	}
	if pin, err := pinLinkDirectory(held); err == nil {
		windows.CloseHandle(pin)
		t.Fatal("moved snapshot admitted by same-object pin")
	}
}

func TestLinkResultHeldParentProviderRootPinnedThroughEffect(t *testing.T) {
	held, err := holdFinalPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if outside, err := held.outsideRoot(root); err != nil || !outside {
		held.release()
		t.Fatalf("outside admission: %v %v", outside, err)
	}
	if err := os.Rename(root, root+"-moved"); err == nil {
		held.release()
		t.Fatal("provider root moved during admitted effect")
	}
	held.release()
	if err := os.Rename(root, root+"-moved"); err != nil {
		t.Fatalf("provider root hold leaked: %v", err)
	}
}

func TestLinkResultHeldParentRefusesLeafFromAnotherFolder(t *testing.T) {
	for _, alias := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "short-alias"}[alias], func(t *testing.T) {
			f := newLinkFixture(t, true)
			original, other := t.TempDir(), t.TempDir()
			name := "long canonical honest destination.txt"
			if err := os.WriteFile(filepath.Join(original, name), []byte("honest"), 0600); err != nil {
				t.Fatal(err)
			}
			if alias {
				decoy := filepath.Join(other, "long canonical decoy destination.txt")
				if err := os.WriteFile(decoy, []byte("decoy"), 0600); err != nil {
					t.Fatal(err)
				}
				short := func(path string) string {
					p, err := windows.UTF16PtrFromString(path)
					if err != nil {
						t.Fatal(err)
					}
					buf := make([]uint16, windows.MAX_LONG_PATH)
					n, err := windows.GetShortPathName(p, &buf[0], uint32(len(buf)))
					if err != nil || n == 0 || int(n) >= len(buf) {
						t.Skipf("8.3 alias unavailable: %v", err)
					}
					return filepath.Base(windows.UTF16ToString(buf[:n]))
				}
				a, b := short(filepath.Join(original, name)), short(decoy)
				if a == name || a != b {
					t.Skipf("matching 8.3 aliases unavailable: %q %q", a, b)
				}
				name = a
			}
			before, err := os.ReadDir(other)
			if err != nil {
				t.Fatal(err)
			}
			saved := holdFinal
			t.Cleanup(func() { holdFinal = saved })
			holdFinal = func(string) (*heldLinkParent, error) { return saved(other) }
			if r := f.link(t, f.local, filepath.Join(original, name)); r.Outcome != api.LinkOutcomeInvalid {
				t.Fatalf("leaf crossed folder identity: %+v", r)
			}
			after, err := os.ReadDir(other)
			if err != nil || len(after) != len(before) {
				t.Fatalf("substituted folder changed: %v %v", after, err)
			}
		})
	}
}

// directoryLink makes link a junction to target with cmd's mklink /J; both
// lie inside the test's own temporary folders.
func directoryLink(t *testing.T, target, link string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v %s", err, out)
	}
}

func TestLinkResult1659PublicLeafGrammar(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, leaf := range []string{"ordinary.", "ordinary ", "NUL", "con.txt"} {
			f := newLinkFixture(t, true)
			parent := t.TempDir()
			destination := filepath.Join(parent, leaf)
			if native {
				destination = `\\?\` + destination
			}
			r := f.link(t, f.local, destination)
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			names := []string{}
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Logf("native=%v leaf=%q outcome=%s actual_entries=%q", native, leaf, r.Outcome, names)
			if leaf == "NUL" || leaf == "con.txt" {
				if r.Outcome != api.LinkOutcomeOtherVolume || len(names) != 0 {
					t.Fatal("reserved destination policy changed", r, names)
				}
			} else if r.Outcome != api.LinkOutcomeLinked || len(names) != 1 {
				t.Fatal("ordinary/native destination refused", r, names)
			}
		}
	}
}
