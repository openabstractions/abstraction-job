package job

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	path := os.Getenv("JOB_PATH")
	if path == "" {
		os.Exit(m.Run())
	}
	n, _ := strconv.Atoi(os.Getenv("JOB_N"))
	if err := mixedRole(os.Getenv("JOB_ROLE"), path, n); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func mixedRole(role, path string, n int) error {
	s, err := NewFileStore(filepath.Dir(filepath.Dir(path)))
	if err != nil {
		return err
	}
	id := strings.TrimSuffix(filepath.Base(path), ".json")
	r, err := s.Load(id)
	if err != nil {
		return err
	}
	epoch := r.Lease.Epoch
	switch role {
	case "writer":
		for range n {
			if _, err := s.Update(id, epoch, func(r *Record) error { r.Progress.Done++; return nil }); err != nil {
				return err
			}
			if _, err := s.Renew(id, epoch, time.Hour); err != nil {
				return err
			}
		}
	case "reader":
		for last := int64(0); last < int64(n); {
			r, err := s.Load(id)
			if err != nil {
				return err
			}
			if r.Progress.Done < last {
				return fmt.Errorf("progress went backwards: %d after %d", r.Progress.Done, last)
			}
			last = r.Progress.Done
		}
	}
	return nil
}
