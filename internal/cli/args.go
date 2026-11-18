package cli

import (
	"errors"
	"flag"
	"io"
	"strings"
	"time"
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

// durationMS reads a duration flag such as --ttl=2h as the milliseconds
// the API takes; "" is 0, which the server answers for a flag it requires.
func durationMS(verb, name, v string) (int64, error) {
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, usagef(verb, "--%s must be a duration such as --%s=2h, got %q", name, name, v)
	}
	return d.Milliseconds(), nil
}

// ttlFlag is the command's --ttl in milliseconds.
func (r *run) ttlFlag(v string) (int64, error) { return durationMS(r.verb, "ttl", v) }

// durationText is an answer's _ms duration as the CLI prints it, such as
// 2h0m0s.
func durationText(ms int64) string { return (time.Duration(ms) * time.Millisecond).String() }
