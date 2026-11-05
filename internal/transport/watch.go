package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"time"

	"github.com/cplieger/grelot/internal/provider"
)

// maxExecFailures is how many non-zero exits in a row, with no exit 0 between,
// end a watcher.
const maxExecFailures = 3

var execTimeout = 10 * time.Second

// Step is what the watcher does after one delivery: go on taking its standing
// wait, or exit with Code.
type Step struct {
	Outcome Outcome
	Code    int
	Exit    bool
}

// Watch runs one layer 2 door, an exec, stdout or exit door, for a watcher.
// It counts an exec door's failures in a row, so it is not safe for
// concurrent use, and a new Watch starts the count again.
type Watch struct {
	door     *provider.Door
	out      io.Writer
	log      *slog.Logger
	failures int
}

// NewWatch returns a Watch writing a stdout or exit door's output to out and
// its WARN and ERROR lines to log, slog.Default() when nil.
func NewWatch(door *provider.Door, out io.Writer, log *slog.Logger) (*Watch, error) {
	switch door.Transport {
	case provider.TransportExec, provider.TransportStdout, provider.TransportExit:
	default:
		return nil, fmt.Errorf("transport: door %s is a %s door, not a watcher's", door.Name, door.Transport)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Watch{door: door, out: out, log: log}, nil
}

// Nudge delivers one nudge; data.Line is the signal line. A stdout door goes
// on and an exit door exits 0. An exec door's exit 0 is Accepted, and a
// non-zero exit or a timeout is RetryLater with one WARN line, except the
// third non-zero exit in a row, which is GiveUp with one ERROR line. A write
// that fails means the host has gone: GiveUp, exit 1. The error is for a
// door that does not render, or ctx ended.
func (w *Watch) Nudge(ctx context.Context, data *provider.Data) (Step, error) {
	if w.door.Transport == provider.TransportExec {
		return w.runExec(ctx, data)
	}
	text, err := w.door.RenderLine(data)
	if err != nil {
		return Step{}, err
	}
	if s, failed := w.print(text); failed {
		return s, nil
	}
	return Step{Outcome: Accepted, Exit: w.door.Transport == provider.TransportExit}, nil
}

// Idle prints a stdout or exit door's idle output, when its hold lapsed with
// no nudge: an exit door then exits 0 and a stdout door goes on. A door with
// no hold has no idle output, and Idle answers an error.
func (w *Watch) Idle(_ context.Context, data *provider.Data) (Step, error) {
	if w.door.Transport == provider.TransportExec {
		return Step{}, fmt.Errorf("transport: door %s is an exec door, which has no idle output", w.door.Name)
	}
	text, err := w.door.RenderIdle(data)
	if err != nil {
		return Step{}, err
	}
	if s, failed := w.print(text); failed {
		return s, nil
	}
	return Step{Outcome: Accepted, Exit: w.door.Transport == provider.TransportExit}, nil
}

// End delivers the end line, data.Line, and always exits: a stdout or exit
// door prints its end output and exits with its end_exit code, and an exec
// door runs once more and exits 1, whatever the run came to.
func (w *Watch) End(ctx context.Context, data *provider.Data) (Step, error) {
	if w.door.Transport == provider.TransportExec {
		s, err := w.runExec(ctx, data)
		if err != nil {
			return Step{}, err
		}
		return Step{Outcome: s.Outcome, Exit: true, Code: 1}, nil
	}
	text, err := w.door.RenderEnd(data)
	if err != nil {
		return Step{}, err
	}
	if s, failed := w.print(text); failed {
		return s, nil
	}
	return Step{Outcome: Accepted, Exit: true, Code: w.door.EndExit}, nil
}

func (w *Watch) print(text string) (Step, bool) {
	if _, err := io.WriteString(w.out, text+"\n"); err != nil {
		w.log.Error("door write failed; this watcher stops", "door", w.door.Name, "error", err)
		return Step{Outcome: GiveUp, Exit: true, Code: 1}, true
	}
	return Step{}, false
}

// runExec runs the door's argv with no shell. Its stdin, stdout and stderr
// are the null device: the watcher's own stdout may be what the host reads,
// and no command output belongs there.
func (w *Watch) runExec(ctx context.Context, data *provider.Data) (Step, error) {
	argv, err := w.door.RenderArgv(data)
	if err != nil {
		return Step{}, err
	}
	runCtx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	err = exec.CommandContext(runCtx, argv[0], argv[1:]...).Run() //nolint:gosec // the argv is the user's provider file, rendered with grelot's own data

	switch {
	case err == nil:
		w.failures = 0
		return Step{Outcome: Accepted}, nil
	case ctx.Err() != nil:
		return Step{}, ctx.Err()
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		w.log.Warn("door command timed out", "door", w.door.Name, "timeout", execTimeout.String())
		return Step{Outcome: RetryLater}, nil
	}
	w.failures++
	attrs := []any{"door", w.door.Name}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		attrs = append(attrs, "exit", ee.ExitCode())
	} else {
		attrs = append(attrs, "error", err.Error())
	}
	if w.failures >= maxExecFailures {
		w.log.Error("door command failed three times in a row; this watcher stops", attrs...)
		return Step{Outcome: GiveUp, Exit: true, Code: 1}, nil
	}
	w.log.Warn("door command failed", append(attrs, "failures", w.failures)...)
	return Step{Outcome: RetryLater}, nil
}
