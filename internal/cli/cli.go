// Package cli runs grelot's client verbs: it reads the command line, the
// client config and the shell it runs in, sends the API calls, and prints
// what an agent reads, the status line first.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cplieger/grelot/internal/client"
	"github.com/cplieger/grelot/internal/proctree"
	"github.com/cplieger/grelot/internal/provider"
)

// Env is what a command reads of the process it runs in.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Lookup reads an environment variable.
	Lookup   func(name string) (string, bool)
	Hostname func() (string, error)
	Getwd    func() (string, error)
	// Home is the user's home directory, for a ~ in a provider's paths.
	Home func() (string, error)
	// Now is the clock of the hooks' marks, time.Now when nil.
	Now func() time.Time
	// Machine is the machine id, <kind>:<hex>, or "" when unknown.
	Machine     func(context.Context) string
	InContainer func() bool
	Providers   *provider.Set
	// Processes is read for this process's ancestors; nil reads none.
	Processes proctree.Table
	// ConfigDir is the user config dir; grelot's files are under its grelot
	// folder.
	ConfigDir string
	Version   string
	PID       int
	// Timeout bounds each API call, client.DefaultTimeout when zero.
	Timeout time.Duration
}

// Exit codes: a refusal or failure is 1, a command line grelot cannot read
// is 2.
const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2
)

// verbs are the commands Run knows, by name. as is added in init, since
// it dispatches through this map.
var verbs = map[string]func(*run, context.Context, []string) error{
	"register":     (*run).register,
	"revive":       (*run).revive,
	"me":           (*run).me,
	"set":          (*run).set,
	"renew":        (*run).renew,
	"bye":          (*run).bye,
	"who":          (*run).who,
	"path":         (*run).path,
	"help":         (*run).help,
	"post":         (*run).post,
	"inbox":        (*run).inbox,
	"history":      (*run).history,
	"receipts":     (*run).receipts,
	"threads":      (*run).threads,
	"follow":       (*run).follow,
	"unfollow":     (*run).unfollow,
	"reserve":      (*run).reserve,
	"release":      (*run).release,
	"reservations": (*run).reservations,
	"delegate":     (*run).delegate,
	"watch":        (*run).watch,
	"wait":         (*run).wait,
	verbHook:       (*run).hook,
}

func init() { verbs["as"] = (*run).as }

// Run runs the command args names, after the global flags --self and
// --provider <name>, and returns the process exit code.
func Run(ctx context.Context, env *Env, args []string) int {
	r := &run{Env: env}
	args, err := r.globals(args)
	if err != nil {
		return r.fail(err)
	}
	if len(args) == 0 {
		return r.fail(usagef("", "name a command: grelot help lists them"))
	}
	fn, ok := verbs[args[0]]
	if !ok {
		return r.fail(usagef("", "unknown command %q: grelot help lists the commands", args[0]))
	}
	r.verb = args[0]
	if err := fn(r, ctx, args[1:]); err != nil {
		if ee, ok := errors.AsType[*exitError](err); ok {
			return ee.code
		}
		if errors.Is(err, errHelp) {
			fmt.Fprint(r.Stdout, section(r.verb))
			return exitOK
		}
		return r.fail(err)
	}
	return exitOK
}

// globals reads the flags before the command.
func (r *run) globals(args []string) ([]string, error) {
	for len(args) > 0 {
		a := args[0]
		switch {
		case a == "--self":
			r.self = true
			args = args[1:]
		case a == "--provider":
			if len(args) < 2 || args[1] == "" {
				return nil, usagef("", "--provider needs a provider name")
			}
			r.providerFlag, args = args[1], args[2:]
		case strings.HasPrefix(a, "--provider="):
			if r.providerFlag = strings.TrimPrefix(a, "--provider="); r.providerFlag == "" {
				return nil, usagef("", "--provider needs a provider name")
			}
			args = args[1:]
		default:
			return args, nil
		}
	}
	return args, nil
}

// usageError is a command line grelot cannot read; it exits 2.
type usageError struct {
	verb string
	msg  string
}

func (e *usageError) Error() string {
	if e.verb == "" {
		return "grelot: " + e.msg
	}
	return fmt.Sprintf("grelot %s: %s; run grelot %s --help", e.verb, e.msg, e.verb)
}

func usagef(verb, format string, a ...any) error {
	return &usageError{verb: verb, msg: fmt.Sprintf(format, a...)}
}

// exitError ends a command with code, its output already printed.
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

// fail prints err and answers its exit code. A refusal leads with the
// status line it carried, unless that line already says the same.
func (r *run) fail(err error) int {
	if ue, ok := errors.AsType[*usageError](err); ok {
		fmt.Fprintln(r.Stderr, ue.Error())
		return exitUsage
	}
	if ae, ok := errors.AsType[*client.APIError](err); ok {
		line := r.statusLine(ae.Status)
		msg := "grelot: " + ae.Msg
		if ae.HTTP == http.StatusUnauthorized {
			msg += fmt.Sprintf(" (%s, or the file %s names), then run grelot me", client.ConfigFile(r.ConfigDir), client.EnvTokenFile)
		}
		if line != "" && line != "grelot: "+ae.Msg && !r.statusShown {
			fmt.Fprintln(r.Stdout, line)
		}
		fmt.Fprintln(r.Stderr, msg)
		return exitFailed
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "grelot") {
		msg = "grelot: " + msg
	}
	fmt.Fprintln(r.Stderr, msg)
	return exitFailed
}
