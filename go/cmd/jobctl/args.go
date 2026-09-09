package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

// parse reads the whole command line or refuses it, and returns the positional
// arguments named by want. A name ending in "..." takes every one that is left.
//
// A flag or an argument a command does not honour is a refusal, never a skip:
// dl accepted `--verify sha256:…`, downloaded, verified nothing and exited 0.
//
// Positionals may sit anywhere among the flags, so parsing resumes after each
// one. The scan this replaced took the first argument not beginning with "-",
// which is a flag's VALUE whenever a flag came first — `claim --owner me job-1`
// claimed a job called "me" and left the id to be read as a flag.
func parse(fs *flag.FlagSet, args []string, want ...string) ([]string, error) {
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			break
		}
		if fs.Arg(0) == "" {
			return nil, fmt.Errorf("%s was given an empty argument", fs.Name())
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	takesRest := len(want) > 0 && strings.HasSuffix(want[len(want)-1], "...")
	switch {
	case len(pos) < len(want):
		return nil, fmt.Errorf("%s needs %s", fs.Name(), strings.TrimSuffix(want[len(pos)], "..."))
	case len(pos) > len(want) && !takesRest:
		return nil, fmt.Errorf("%s does not know what to do with %q", fs.Name(), pos[len(want)])
	}
	// An empty value is the same lie as an unknown flag: `--by "$WHO"` with WHO
	// unset would substitute a generated name and report it as the caller's.
	var blank string
	fs.Visit(func(f *flag.Flag) {
		if f.Value.String() == "" {
			blank = f.Name
		}
	})
	if blank != "" {
		return nil, fmt.Errorf("-%s wants a value", blank)
	}
	return pos, nil
}
