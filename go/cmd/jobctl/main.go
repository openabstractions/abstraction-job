// jobctl drives the job store from a shell, so the cross-language conformance
// test can be an actual script that runs both implementations against one
// directory rather than a mock of one talking to a mock of the other.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
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
		cmdOrphans(s, os.Args[2:])
	case "-h", "--help", "help":
		usage()
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

// need parses or ends the process. A command line this tool will not act on
// exits 2 — nothing was attempted, which is a different thing for a script to
// know than a job that could not be claimed. See download/CONTRACT.md
// § What a status byte can carry.
func need(fs *flag.FlagSet, args []string, want ...string) []string {
	pos, err := parse(fs, args, want...)
	if errors.Is(err, flag.ErrHelp) {
		usage()
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "jobctl:", err)
		os.Exit(2)
	}
	return pos
}

// cmdRecall asks the holder for the lease back. The epoch is the one the caller
// SAW, not one it holds: a third party recalling a residency it has only read.
func cmdRecall(s job.Store, args []string) {
	fs := flag.NewFlagSet("recall", flag.ContinueOnError)
	epoch := fs.Int64("epoch", 0, "the epoch the recall was decided against")
	reason := fs.String("reason", "", "why, in words the holder's kind can act on")
	grace := fs.Float64("grace", 30, "seconds the holder has before the lease lapses")
	by := fs.String("by", "", "who is asking")
	id := need(fs, args, "a job id")[0]
	if *by == "" {
		host, _ := os.Hostname()
		*by = fmt.Sprintf("jobctl@%s:%d", host, os.Getpid())
	}
	rec, err := s.Recall(id, *epoch, *reason, *by, time.Duration(*grace*float64(time.Second)))
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

// jobctl is deliberately ignorant of what a job IS. It takes a kind and a spec
// as raw JSON and never looks inside — which is the same contract the job
// package itself keeps, and the reason a download can grow new spec fields
// without this tool, or the Python one, needing to know.
func cmdSubmit(s job.Store, args []string) {
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	kind := fs.String("kind", "", "what this job is; who can read the spec")
	spec := fs.String("spec", "", "the job's spec, as raw JSON")
	total := fs.Int64("total", 0, "expected total work, in the kind's own units")
	requires := fs.String("requires", "", "comma-separated capabilities an implementation must have")
	need(fs, args)

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
	fs := flag.NewFlagSet("claim", flag.ContinueOnError)
	owner := fs.String("owner", "", "who is taking it")
	ttl := fs.Float64("ttl", 30, "how long the lease lasts, in seconds")
	id := need(fs, args, "a job id")[0]
	r, err := s.Claim(id, *owner, time.Duration(*ttl*float64(time.Second)))
	if err != nil {
		fatal(err)
	}
	// The epoch and the predecessor's checkpoint are what a new owner needs.
	fmt.Printf("epoch=%d state=%s checkpoint=%s\n", r.Lease.Epoch, r.State, compact(r.Checkpoint))
}

func cmdProgress(s job.Store, args []string) {
	fs := flag.NewFlagSet("progress", flag.ContinueOnError)
	epoch := fs.Int64("epoch", 0, "the epoch this owner holds")
	done := fs.Int64("done", 0, "work done, in the kind's own units")
	checkpoint := fs.String("checkpoint", "", "what a successor needs to resume, as raw JSON")
	id := need(fs, args, "a job id")[0]
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
	fs := flag.NewFlagSet("finish", flag.ContinueOnError)
	epoch := fs.Int64("epoch", 0, "the epoch this owner holds")
	state := fs.String("state", string(job.StateTransferred), "transferred|complete|failed")
	id := need(fs, args, "a job id")[0]
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
	id := need(flag.NewFlagSet("show", flag.ContinueOnError), args, "a job id")[0]
	r, err := s.Load(id)
	if err != nil {
		fatal(err)
	}
	b, err := r.Encode()
	if err != nil {
		fatal(err)
	}
	fmt.Print(string(b))
}

// cmdOrphans prints what the sweep found AND what it could not read, then exits
// non-zero if anything was unreadable.
//
// Failing on the error alone would hide every orphan the store could read, which
// is how a sweep that is half blind ends up reporting nothing at all.
func cmdOrphans(s job.Store, args []string) {
	need(flag.NewFlagSet("orphans", flag.ContinueOnError), args)
	rs, err := s.Orphans()
	var unread *job.ErrUnreadable
	if err != nil && !errors.As(err, &unread) {
		fatal(err)
	}
	for _, r := range rs {
		fmt.Printf("%s kind=%s state=%s checkpoint=%s\n", r.ID, r.Kind, r.State, compact(r.Checkpoint))
	}
	if unread != nil {
		fmt.Fprintln(os.Stderr, "jobctl:", unread)
		os.Exit(1)
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
	id := need(flag.NewFlagSet("cancel", flag.ContinueOnError), args, "a job id")[0]
	host, _ := os.Hostname()
	owner := fmt.Sprintf("jobctl@%s:%d", host, os.Getpid())

	h := job.Open(s, id, owner)
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
	fs := flag.NewFlagSet("intent", flag.ContinueOnError)
	by := fs.String("by", "", "who is asking")
	pos := need(fs, args, "a job id", "run|pause|cancel")
	if *by == "" {
		host, _ := os.Hostname()
		*by = fmt.Sprintf("jobctl@%s:%d", host, os.Getpid())
	}
	rec, err := s.SetIntent(pos[0], job.Want(pos[1]), *by)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("%s %s\n", rec.ID, rec.Wants())
}
