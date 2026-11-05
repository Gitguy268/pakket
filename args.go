package main

import (
	"flag"
	"fmt"
	"strings"
)

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// parseAll parses flags anywhere among the arguments, like most CLIs do.
// Everything after "--" is positional.
func parseAll(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		var rest []string
		for i, a := range args {
			if a == "--" {
				rest = args[i+1:]
				args = args[:i]
				break
			}
		}
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		left := fs.Args()
		if len(left) == 0 {
			return append(pos, rest...), nil
		}
		pos = append(pos, left[0])
		args = left[1:]
		if len(args) == 0 {
			return append(pos, rest...), nil
		}
		if rest != nil {
			args = append(args, "--")
			args = append(args, rest...)
		}
	}
}

// alias registers a second name for an existing flag.
func alias(fs *flag.FlagSet, short, long string) {
	f := fs.Lookup(long)
	if f == nil {
		panic(fmt.Sprintf("no flag %s", long))
	}
	fs.Var(f.Value, short, "short for --"+long)
}
