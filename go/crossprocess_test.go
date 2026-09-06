package job

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// A record another process wrote is seen by a watcher here within one poll
// plus slack, on a local disk. This pins the file binding having no cache of
// its own: on 2026-09-06 a long-lived supervisor read a record 154 s after a
// fresh process could, over SMB, and the first question was whether the delay
// was ours.
func TestAnotherProcessesWriteIsSeenWithinOnePoll(t *testing.T) {
	if root := os.Getenv("JOB_CROSSPROCESS_STORE"); root != "" {
		s, err := NewFileStore(root)
		if err != nil {
			t.Fatal(err)
		}
		id := os.Getenv("JOB_CROSSPROCESS_ID")
		rec, err := s.Claim(id, "the-other-process", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Update(id, rec.Lease.Epoch, func(r *Record) error {
			r.Progress.Done = 42
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return
	}

	root := t.TempDir()
	s, err := NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Submit(Record{Kind: "test", Spec: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	sub := Watch(s, "test")
	defer sub.Close()
	if _, err := sub.Next(context.Background()); err != nil {
		t.Fatal(err)
	}

	child := exec.Command(os.Args[0], "-test.run=^TestAnotherProcessesWriteIsSeenWithinOnePoll$")
	child.Env = append(os.Environ(), "JOB_CROSSPROCESS_STORE="+root, "JOB_CROSSPROCESS_ID="+id)
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("the other process: %v\n%s", err, out)
	}
	written := time.Now()

	const bound = pollEvery + 2*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	for {
		n, err := sub.Next(ctx)
		if err != nil {
			t.Fatalf("a write by another process was still not visible after %s", bound)
		}
		for _, r := range n.Records {
			if r.ID == id && r.Progress.Done == 42 {
				t.Logf("seen %s after the other process exited", time.Since(written).Round(time.Millisecond))
				return
			}
		}
	}
}
