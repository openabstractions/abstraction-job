// jobctl drives the job store from a shell, so the cross-language conformance
// test can be an actual script that runs both implementations against one
// directory rather than a mock of one talking to a mock of the other.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	job "github.com/ReinisLusis/abstraction-job"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	root := os.Getenv("JOB_STORE")
	if root == "" {
		fatal(fmt.Errorf("JOB_STORE is not set"))
	}
	s, err := job.NewFileStore(root)
	if err != nil {
		fatal(err)
	}

	switch os.Args[1] {
	case "submit":
		cmdSubmit(s, os.Args[2:])
	case "claim":
		cmdClaim(s, os.Args[2:])
	case "progress":
		cmdProgress(s, os.Args[2:])
	case "finish":
		cmdFinish(s, os.Args[2:])
	case "show":
		cmdShow(s, os.Args[2:])
	case "orphans":
		cmdOrphans(s)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println("usage: jobctl <submit|claim|progress|finish|show|orphans> [args]   (JOB_STORE must be set)")
	fmt.Println("  submit --kind K --spec '<json>' [--total N] [--requires a,b]")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "jobctl:", err)
	os.Exit(1)
}

// splitID pulls the first non-flag argument out, so `claim <id> --owner x` and
// `claim --owner x <id>` both work. Go's flag package stops at the first
// positional, and a CLI that silently ignores half its flags depending on
// argument order is a trap for exactly the shell scripts this exists to serve.
func splitID(args []string) (string, []string) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a, append(append([]string{}, args[:i]...), args[i+1:]...)
		}
	}
	return "", args
}

// jobctl is deliberately ignorant of what a job IS. It takes a kind and a spec
// as raw JSON and never looks inside — which is the same contract the job
// package itself keeps, and the reason a download can grow new spec fields
// without this tool, or the Python one, needing to know.
func cmdSubmit(s *job.FileStore, args []string) {
	fs := flag.NewFlagSet("submit", flag.ExitOnError)
	kind := fs.String("kind", "", "what this job is; who can read the spec")
	spec := fs.String("spec", "", "the job's spec, as raw JSON")
	total := fs.Int64("total", 0, "expected total work, in the kind's own units")
	requires := fs.String("requires", "", "comma-separated capabilities an implementation must have")
	fs.Parse(args)

	rec := job.Record{Kind: *kind, Spec: json.RawMessage(*spec)}
	rec.Progress.Total = *total
	if *requires != "" {
		rec.Requires = strings.Split(*requires, ",")
	}
	id, err := s.Submit(rec)
	if err != nil {
		fatal(err)
	}
	fmt.Println(id)
}

func cmdClaim(s *job.FileStore, args []string) {
	id, rest := splitID(args)
	fs := flag.NewFlagSet("claim", flag.ExitOnError)
	owner := fs.String("owner", "", "who is taking it")
	ttl := fs.Duration("ttl", 30*time.Second, "how long the lease lasts")
	fs.Parse(rest)
	if id == "" {
		fatal(fmt.Errorf("claim needs a job id"))
	}
	r, err := s.Claim(id, *owner, *ttl)
	if err != nil {
		fatal(err)
	}
	// The epoch and the predecessor's checkpoint are what a new owner needs.
	cp := "none"
	if len(r.Checkpoint) > 0 {
		cp = string(r.Checkpoint)
	}
	fmt.Printf("epoch=%d state=%s checkpoint=%s\n", r.Lease.Epoch, r.State, cp)
}

func cmdProgress(s *job.FileStore, args []string) {
	id, rest := splitID(args)
	fs := flag.NewFlagSet("progress", flag.ExitOnError)
	epoch := fs.Int64("epoch", 0, "the epoch this owner holds")
	done := fs.Int64("done", 0, "work done, in the kind's own units")
	checkpoint := fs.String("checkpoint", "", "what a successor needs to resume, as raw JSON")
	fs.Parse(rest)
	if id == "" {
		fatal(fmt.Errorf("progress needs a job id"))
	}
	r, err := s.Update(id, *epoch, func(r *job.Record) error {
		r.Progress.Done = *done
		r.Progress.UpdatedAt = time.Now().UTC()
		if *checkpoint != "" {
			r.Checkpoint = json.RawMessage(*checkpoint)
		}
		return nil
	})
	if err != nil {
		fatal(err)
	}
	fmt.Printf("done=%d checkpoint=%s\n", r.Progress.Done, string(r.Checkpoint))
}

func cmdFinish(s *job.FileStore, args []string) {
	id, rest := splitID(args)
	fs := flag.NewFlagSet("finish", flag.ExitOnError)
	epoch := fs.Int64("epoch", 0, "the epoch this owner holds")
	state := fs.String("state", string(job.StateTransferred), "transferred|complete|failed")
	fs.Parse(rest)
	if id == "" {
		fatal(fmt.Errorf("finish needs a job id"))
	}
	r, err := s.Update(id, *epoch, func(r *job.Record) error {
		st := job.State(*state)
		if !st.Valid() {
			return fmt.Errorf("invalid state %q", *state)
		}
		r.State = st
		return nil
	})
	if err != nil {
		fatal(err)
	}
	fmt.Printf("state=%s\n", r.State)
}

func cmdShow(s *job.FileStore, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("show needs a job id"))
	}
	r, err := s.Load(args[0])
	if err != nil {
		fatal(err)
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	fmt.Println(string(b))
}

func cmdOrphans(s *job.FileStore) {
	rs, err := s.Orphans()
	if err != nil {
		fatal(err)
	}
	for _, r := range rs {
		fmt.Printf("%s kind=%s state=%s checkpoint=%s\n", r.ID, r.Kind, r.State, string(r.Checkpoint))
	}
}
