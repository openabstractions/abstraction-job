// jobctl drives the job store from a shell, so the cross-language conformance
// test can be an actual script that runs both implementations against one
// directory rather than a mock of one talking to a mock of the other.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	job "github.com/openabstractions/abstraction-job/go"
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
	case "intent":
		cmdIntent(s, os.Args[2:])
	case "recall":
		cmdRecall(s, os.Args[2:])
	case "cancel":
		cmdCancel(s, os.Args[2:])
	case "orphans":
		cmdOrphans(s)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println("usage: jobctl <submit|claim|progress|finish|show|cancel|intent|recall|orphans> [args]   (JOB_STORE must be set)")
	fmt.Println("  submit --kind K --spec '<json>' [--total N] [--requires a,b]")
	fmt.Println("  recall <id> --epoch N --reason WHY [--grace SECONDS] [--by who]")
}

// cmdRecall asks the holder for the lease back. The epoch is the one the caller
// SAW, not one it holds: a third party recalling a residency it has only read.
func cmdRecall(s job.Store, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: jobctl recall <id> --epoch N --reason WHY [--grace SECONDS] [--by who]"))
	}
	fs := flag.NewFlagSet("recall", flag.ExitOnError)
	epoch := fs.Int64("epoch", 0, "the epoch the recall was decided against")
	reason := fs.String("reason", "", "why, in words the holder's kind can act on")
	grace := fs.Float64("grace", 30, "seconds the holder has before the lease lapses")
	by := fs.String("by", "", "who is asking")
	fs.Parse(args[1:])
	if *by == "" {
		host, _ := os.Hostname()
		*by = fmt.Sprintf("jobctl@%s:%d", host, os.Getpid())
	}
	rec, err := s.Recall(args[0], *epoch, *reason, *by, time.Duration(*grace*float64(time.Second)))
	if err != nil {
		fatal(err)
	}
	fmt.Printf("%s recalled until %s\n", rec.ID, rec.Lease.Recall.Until.Format(time.RFC3339))
}

// compact puts raw JSON on one line. The record on disk is indented for humans,
// so a checkpoint read back out carries newlines — and this output is a
// conformance surface that a harness parses, where "same value, different
// whitespace" counts as two implementations disagreeing.
func compact(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "none"
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
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
func cmdSubmit(s job.Store, args []string) {
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

func cmdClaim(s job.Store, args []string) {
	id, rest := splitID(args)
	fs := flag.NewFlagSet("claim", flag.ExitOnError)
	owner := fs.String("owner", "", "who is taking it")
	ttl := fs.Float64("ttl", 30, "how long the lease lasts, in seconds")
	fs.Parse(rest)
	if id == "" {
		fatal(fmt.Errorf("claim needs a job id"))
	}
	r, err := s.Claim(id, *owner, time.Duration(*ttl*float64(time.Second)))
	if err != nil {
		fatal(err)
	}
	// The epoch and the predecessor's checkpoint are what a new owner needs.
	fmt.Printf("epoch=%d state=%s checkpoint=%s\n", r.Lease.Epoch, r.State, compact(r.Checkpoint))
}

func cmdProgress(s job.Store, args []string) {
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
		r.Progress.UpdatedAt = job.At(time.Now())
		if *checkpoint != "" {
			r.Checkpoint = json.RawMessage(*checkpoint)
		}
		return nil
	})
	if err != nil {
		fatal(err)
	}
	fmt.Printf("done=%d checkpoint=%s\n", r.Progress.Done, compact(r.Checkpoint))
}

func cmdFinish(s job.Store, args []string) {
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

func cmdShow(s job.Store, args []string) {
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

func cmdOrphans(s job.Store) {
	rs, err := s.Orphans()
	if err != nil {
		fatal(err)
	}
	for _, r := range rs {
		fmt.Printf("%s kind=%s state=%s checkpoint=%s\n", r.ID, r.Kind, r.State, compact(r.Checkpoint))
	}
}

// cmdCancel abandons a job from outside, which is the operation a person
// performs and a worker does not.
//
// It goes through job.Open rather than reaching for the store directly, so this
// command exercises the same handle an application uses — including its refusal
// when somebody currently holds the lease, which is a real limit of schema 3
// rather than something to work around here.
func cmdCancel(s job.Store, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: jobctl cancel <id>"))
	}
	host, _ := os.Hostname()
	owner := fmt.Sprintf("jobctl@%s:%d", host, os.Getpid())

	h := job.Open(s, args[0], owner)
	if err := h.Cancel(); err != nil {
		fatal(err)
	}
	rec, err := h.Record()
	if err != nil {
		fatal(err)
	}
	fmt.Printf("%s %s\n", rec.ID, rec.State)
}

// cmdIntent says what should happen, without holding the lease.
//
// A command rather than a flag on claim, because the whole point is that the
// caller is NOT the worker: no epoch is presented and none is needed.
func cmdIntent(s job.Store, args []string) {
	if len(args) < 2 {
		fatal(fmt.Errorf("usage: jobctl intent <id> <run|pause|cancel> [--by who]"))
	}
	by := ""
	for i := 2; i+1 < len(args); i++ {
		if args[i] == "--by" {
			by = args[i+1]
		}
	}
	if by == "" {
		host, _ := os.Hostname()
		by = fmt.Sprintf("jobctl@%s:%d", host, os.Getpid())
	}
	rec, err := s.SetIntent(args[0], job.Want(args[1]), by)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("%s %s\n", rec.ID, rec.Wants())
}
