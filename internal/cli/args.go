package cli

import (
	"errors"
	"flag"
	"io"
	"strings"
)

// flags is a verb's flag set, quiet so the verb prints its own errors.
func flags(verb string) *flag.FlagSet {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// errHelp asks for a verb's section of the manual.
var errHelp = errors.New("help")

// parse reads fs's flags anywhere in args, since agents write the name
// before the flags, and answers the other arguments in order.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var rest []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, errHelp
			}
			msg := err.Error()
			if name, ok := strings.CutPrefix(msg, "flag provided but not defined: "); ok {
				msg = "unknown flag " + name
			}
			return nil, usagef(fs.Name(), "%s", msg)
		}
		args = fs.Args()
		if len(args) == 0 {
			return rest, nil
		}
		rest, args = append(rest, args[0]), args[1:]
	}
}

// list is a flag that may repeat and holds comma-separated values.
type list []string

func (l *list) String() string { return strings.Join(*l, ",") }

func (l *list) Set(v string) error {
	for p := range strings.SplitSeq(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}
