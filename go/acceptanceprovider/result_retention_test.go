package acceptanceprovider

import (
	"context"
	"testing"

	job "github.com/openabstractions/abstraction-job/go"
)

type retainingExecutor struct {
	testExecutor
	retention int64
}

func (retainingExecutor) ReadOperationResult(string, *job.Record, int64, int64) ([]byte, int64, error) {
	return nil, 0, ErrResultLost
}
func (e retainingExecutor) ResultRetentionMs() int64 { return e.retention }

type readerOnlyExecutor struct{ testExecutor }

func (readerOnlyExecutor) ReadOperationResult(string, *job.Record, int64, int64) ([]byte, int64, error) {
	return nil, 0, ErrResultLost
}

var _ = context.Background

// The history window declares result retention only for a provider that serves
// results and declares a positive retention [JOB-A11].
func TestHistoryWindowDeclaresResultRetention(t *testing.T) {
	for name, c := range map[string]struct {
		open func(root string) (*Provider, error)
		want int64
	}{
		"admission-only": {func(root string) (*Provider, error) { return Open(root, "owner") }, 0},
		"reader-without-declaration": {func(root string) (*Provider, error) {
			return OpenWithExecutor(root, "owner", readerOnlyExecutor{testExecutor{profile: "reader-v1"}})
		}, 0},
		"declared": {func(root string) (*Provider, error) {
			return OpenWithExecutor(root, "owner", retainingExecutor{testExecutor{profile: "retain-v1"}, 3_600_000})
		}, 3_600_000},
		"negative-declaration": {func(root string) (*Provider, error) {
			return OpenWithExecutor(root, "owner", retainingExecutor{testExecutor{profile: "retain-neg-v1"}, -5})
		}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			p, err := c.open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			w, err := p.Bind("alice").GetHistoryWindow()
			if err != nil || w.ResultRetentionMs != c.want || w.MinimumRetentionMs != MinimumRetentionMs {
				t.Fatalf("history window: %+v %v", w, err)
			}
		})
	}
}
