package main

import (
	"errors"
	"flag"
	"testing"
)

func intentFlags() (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("intent", flag.ContinueOnError)
	return fs, fs.String("by", "", "who is asking")
}

func claimFlags() (*flag.FlagSet, *string, *float64) {
	fs := flag.NewFlagSet("claim", flag.ContinueOnError)
	return fs, fs.String("owner", "", "who is taking it"), fs.Float64("ttl", 30, "seconds")
}

func TestParseRefuses(t *testing.T) {
	for _, c := range []struct {
		name string
		fs   *flag.FlagSet
		args []string
		want []string
	}{
		{"intent with a flag it does not honour", newIntent(), []string{"j1", "pause", "--bye", "me"}, []string{"a job id", "run|pause|cancel"}},
		{"intent --by with nothing after it", newIntent(), []string{"j1", "pause", "--by"}, []string{"a job id", "run|pause|cancel"}},
		{"intent --by given an empty value", newIntent(), []string{"j1", "pause", "--by", ""}, []string{"a job id", "run|pause|cancel"}},
		{"intent with a third positional", newIntent(), []string{"j1", "pause", "extra"}, []string{"a job id", "run|pause|cancel"}},
		{"intent missing the want", newIntent(), []string{"j1"}, []string{"a job id", "run|pause|cancel"}},
		{"claim with a second positional", newClaim(), []string{"j1", "j2"}, []string{"a job id"}},
		{"claim with an empty id", newClaim(), []string{""}, []string{"a job id"}},
		{"orphans given an argument", flag.NewFlagSet("orphans", flag.ContinueOnError), []string{"j1"}, nil},
		{"show given two ids", flag.NewFlagSet("show", flag.ContinueOnError), []string{"j1", "j2"}, []string{"a job id"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := parse(c.fs, c.args, c.want...); err == nil {
				t.Fatal("accepted; an argument the command will not act on must be a refusal")
			}
		})
	}
}

// A flag's VALUE is not a positional. The scan this replaced returned "me" as
// the job id here, so `claim --owner me j1` claimed a job nobody had submitted.
func TestParseDoesNotMistakeAValueForAnID(t *testing.T) {
	fs, owner, ttl := claimFlags()
	pos, err := parse(fs, []string{"--owner", "me", "j1"}, "a job id")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pos[0] != "j1" || *owner != "me" || *ttl != 30 {
		t.Fatalf("id=%q owner=%q ttl=%v, want j1 me 30", pos[0], *owner, *ttl)
	}
}

// --by last used to be dropped and a generated name recorded in its place, so
// the store said jobctl asked for the pause when a person had.
func TestParseKeepsATrailingFlag(t *testing.T) {
	fs, by := intentFlags()
	pos, err := parse(fs, []string{"j1", "pause", "--by", "lemonade-ui"}, "a job id", "run|pause|cancel")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pos[0] != "j1" || pos[1] != "pause" || *by != "lemonade-ui" {
		t.Fatalf("got %q %q by=%q", pos[0], pos[1], *by)
	}
}

func TestParseAcceptsFlagsAroundPositionals(t *testing.T) {
	fs, by := intentFlags()
	pos, err := parse(fs, []string{"j1", "--by", "ui", "pause"}, "a job id", "run|pause|cancel")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pos[0] != "j1" || pos[1] != "pause" || *by != "ui" {
		t.Fatalf("got %q %q by=%q", pos[0], pos[1], *by)
	}
}

func TestParseTakesTheRestWhenAsked(t *testing.T) {
	fs := flag.NewFlagSet("names", flag.ContinueOnError)
	pos, err := parse(fs, []string{"a", "b", "c"}, "a name...")
	if err != nil || len(pos) != 3 {
		t.Fatalf("pos=%v err=%v", pos, err)
	}
	if _, err := parse(flag.NewFlagSet("names", flag.ContinueOnError), nil, "a name..."); err == nil {
		t.Fatal("accepted no names at all")
	}
}

func TestParseNamesWhyItRefused(t *testing.T) {
	if _, err := parse(flag.NewFlagSet("show", flag.ContinueOnError), []string{"-h"}, "a job id"); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("err = %v, want flag.ErrHelp", err)
	}
}

func newIntent() *flag.FlagSet { fs, _ := intentFlags(); return fs }
func newClaim() *flag.FlagSet  { fs, _, _ := claimFlags(); return fs }
