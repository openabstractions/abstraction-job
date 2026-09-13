package acceptanceprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	job "github.com/openabstractions/abstraction-job/go"
)

type testExecutor struct {
	profile, prefix string
	refuse          bool
}

func (e testExecutor) Profile() string { return e.profile }
func (e testExecutor) Prepare(id, kind string, spec []byte) ([]byte, error) {
	if e.refuse {
		return nil, errors.New("unsupported")
	}
	return json.Marshal(map[string]string{"private_destination": e.prefix + id})
}
func (testExecutor) Serve(context.Context, job.Store) error { return nil }

func TestExecutionPreparationRecoversEveryAdmissionBoundary(t *testing.T) {
	for _, point := range []string{"after-journal", "after-job", "before-reply"} {
		t.Run(point, func(t *testing.T) {
			root := t.TempDir()
			executor := testExecutor{profile: "test-v1", prefix: "private/"}
			p, err := OpenWithExecutor(root, "logical-owner", executor)
			if err != nil {
				t.Fatal(err)
			}
			s := submission(p, "stable")
			fired := false
			p.fault = func(at string) error {
				if at == point {
					fired = true
					return errors.New("interrupted")
				}
				return nil
			}
			if result, err := p.Bind("alice").Submit(s); err != nil || result.Outcome != "unknown" {
				t.Fatalf("fault response: %+v %v", result, err)
			}
			if !fired {
				t.Fatal("admission boundary was not reached")
			}
			p, err = OpenWithExecutor(root, "logical-owner", executor)
			if err != nil {
				t.Fatal(err)
			}
			r := accept(t, p.Bind("alice"), s)
			rec, err := p.store.Load(r.OperationId)
			if err != nil {
				t.Fatal(err)
			}
			expected, _ := executor.Prepare(r.OperationId, s.Kind, s.Spec)
			var got, want any
			if json.Unmarshal(rec.Spec, &got) != nil || json.Unmarshal(expected, &want) != nil {
				t.Fatal("invalid prepared payload")
			}
			canonicalGot, _ := json.Marshal(got)
			canonicalWant, _ := json.Marshal(want)
			if !bytes.Equal(canonicalGot, canonicalWant) || jobs(t, root) != 1 {
				t.Fatal("recovery changed prepared work")
			}
			if _, err := Open(root, "logical-owner"); err == nil {
				t.Fatal("admission-only downgrade accepted")
			}
			if _, err := OpenWithExecutor(root, "logical-owner", testExecutor{profile: "test-v2"}); err == nil {
				t.Fatal("profile replacement accepted")
			}
			if _, err := OpenWithExecutor(root, "logical-owner", testExecutor{profile: "test-v1", prefix: "changed/"}); err == nil {
				t.Fatal("changed transformation accepted")
			}
		})
	}
}

func TestExecutionRefusalSealsIdentity(t *testing.T) {
	root := t.TempDir()
	p, err := OpenWithExecutor(root, "logical-owner", testExecutor{profile: "test-v1", refuse: true})
	if err != nil {
		t.Fatal(err)
	}
	s := submission(p, "refused")
	v, err := p.Bind("alice").Submit(s)
	if err != nil || v.Receipt != nil || jobs(t, root) != 0 {
		t.Fatalf("refusal: %+v %v", v, err)
	}
	p, err = OpenWithExecutor(root, "logical-owner", testExecutor{profile: "test-v1"})
	if err != nil {
		t.Fatal(err)
	}
	v, err = p.Bind("alice").Submit(s)
	if err != nil || v.Receipt != nil || jobs(t, root) != 0 {
		t.Fatalf("sealed identity accepted: %+v %v", v, err)
	}
}

func TestExecutionRefusesUnownedLegacyStore(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "jobs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "jobs", "legacy.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenWithExecutor(root, "logical-owner", testExecutor{profile: "test-v1"}); err == nil {
		t.Fatal("unowned work adopted")
	}
}
