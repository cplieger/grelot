package transport

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/provider"
)

// lineFile is a provider file with one door of transport; extra is spliced
// into the door.
func lineFile(transport, extra string) string {
	return "name: testhost\ndoors:\n  - name: watch\n    layer: 2\n    transport: " + transport + "\n" + extra
}

// logLines is a JSON slog handler's output, one record per line.
type logLines struct{ bytes.Buffer }

func (l *logLines) logger() *slog.Logger { return slog.New(slog.NewJSONHandler(l, nil)) }

func (l *logLines) records(t *testing.T, level string) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(l.Bytes()))
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("log line %q is not JSON: %v", sc.Text(), err)
		}
		if rec["level"] == level {
			out = append(out, rec)
		}
	}
	return out
}

// fakeExec is an exec door running the test binary as its program. mode sets
// what the next run does; runs lists each run's arguments.
type fakeExec struct {
	door   *provider.Door
	ctl    string
	record string
}

func newFakeExec(t *testing.T) *fakeExec {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	dir := t.TempDir()
	f := &fakeExec{ctl: filepath.Join(dir, "ctl"), record: filepath.Join(dir, "record")}
	f.door = loadDoor(t, lineFile("exec", "    argv: ['"+bin+"', '"+fakeDoorArg+"', '"+f.ctl+"', '"+f.record+"', '{{.Line}}', '{{.Session}}']\n"))
	return f
}

func (f *fakeExec) mode(t *testing.T, mode string) {
	t.Helper()
	if err := os.WriteFile(f.ctl, []byte(mode), 0o600); err != nil {
		t.Fatalf("write ctl: %v", err)
	}
}

func (f *fakeExec) runs(t *testing.T) [][]string {
	t.Helper()
	data, err := os.ReadFile(f.record)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var out [][]string
	for line := range strings.Lines(string(data)) {
		var args []string
		if err := json.Unmarshal([]byte(line), &args); err != nil {
			t.Fatalf("record line %q: %v", line, err)
		}
		out = append(out, args)
	}
	return out
}

func newWatch(t *testing.T, door *provider.Door, out *bytes.Buffer, logs *logLines) *Watch {
	t.Helper()
	w, err := NewWatch(door, out, logs.logger())
	if err != nil {
		t.Fatalf("NewWatch: %v", err)
	}
	return w
}

func watchData(line string) *provider.Data {
	return &provider.Data{Line: line, Seq: 7, Session: "sess 1; echo hi", Identity: "test-agent"}
}

// ok fails the test on a delivery's error and passes its Step through.
func ok(t *testing.T) func(Step, error) Step {
	t.Helper()
	return func(s Step, err error) Step {
		t.Helper()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return s
	}
}

var (
	keepGoing = Step{Outcome: Accepted}
	retry     = Step{Outcome: RetryLater}
	giveUp    = Step{Outcome: GiveUp, Exit: true, Code: 1}
)

func TestExecAccepted(t *testing.T) {
	f := newFakeExec(t)
	f.mode(t, "ok")
	var out bytes.Buffer
	var logs logLines
	w := newWatch(t, f.door, &out, &logs)

	line := SignalLine(7, 2)
	if got := ok(t)(w.Nudge(t.Context(), watchData(line))); got != keepGoing {
		t.Errorf("Nudge = %+v, want %+v", got, keepGoing)
	}
	runs := f.runs(t)
	if len(runs) != 1 || !slices.Equal(runs[0], []string{line, "sess 1; echo hi"}) {
		t.Errorf("runs = %q, want one run with the line and the session as two arguments, no shell", runs)
	}
	if out.Len() != 0 {
		t.Errorf("the watcher's stdout got %q; the command's output must not reach it", out.String())
	}
	if n := len(logs.records(t, "WARN")) + len(logs.records(t, "ERROR")); n != 0 {
		t.Errorf("logged %d WARN or ERROR lines, want 0", n)
	}
}

func TestExecFailures(t *testing.T) {
	f := newFakeExec(t)
	f.mode(t, "fail")
	var out bytes.Buffer
	var logs logLines
	w := newWatch(t, f.door, &out, &logs)

	for i := 1; i <= 2; i++ {
		if got := ok(t)(w.Nudge(t.Context(), watchData(SignalLine(7, 1)))); got != retry {
			t.Fatalf("failure %d: Nudge = %+v, want %+v", i, got, retry)
		}
		warns := logs.records(t, "WARN")
		if len(warns) != i || warns[i-1]["exit"] != float64(3) || warns[i-1]["door"] != "watch" {
			t.Fatalf("failure %d: WARN lines = %v, want %d, the last naming the door and exit 3", i, warns, i)
		}
	}
	if got := ok(t)(w.Nudge(t.Context(), watchData(SignalLine(7, 1)))); got != giveUp {
		t.Errorf("third failure: Nudge = %+v, want %+v", got, giveUp)
	}
	if errs := logs.records(t, "ERROR"); len(errs) != 1 || errs[0]["exit"] != float64(3) {
		t.Errorf("ERROR lines = %v, want one with exit 3", errs)
	}
	if n := len(logs.records(t, "WARN")); n != 2 {
		t.Errorf("WARN lines = %d after the third failure, want still 2", n)
	}
}

func TestExecSuccessResetsFailures(t *testing.T) {
	f := newFakeExec(t)
	var out bytes.Buffer
	var logs logLines
	w := newWatch(t, f.door, &out, &logs)
	nudge := func(mode string) Step {
		f.mode(t, mode)
		return ok(t)(w.Nudge(t.Context(), watchData(SignalLine(7, 1))))
	}
	for i, mode := range []string{"fail", "fail", "ok", "fail", "fail"} {
		if got := nudge(mode); got.Exit {
			t.Fatalf("run %d (%s): Nudge = %+v, want the watcher to go on", i+1, mode, got)
		}
	}
	if got := nudge("fail"); got != giveUp {
		t.Errorf("third failure after the success: Nudge = %+v, want %+v", got, giveUp)
	}
}

// Not parallel: it shortens the shipped command timeout. A timeout is retry
// later and neither counts as nor clears a failure. Only the sleeping run
// gets the short timeout: a process start can take over a second on a
// managed Windows machine, so the failing runs keep the shipped one.
func TestExecTimeout(t *testing.T) {
	shipped := execTimeout
	t.Cleanup(func() { execTimeout = shipped })
	f := newFakeExec(t)
	var out bytes.Buffer
	var logs logLines
	w := newWatch(t, f.door, &out, &logs)
	nudge := func(mode string) Step {
		f.mode(t, mode)
		return ok(t)(w.Nudge(t.Context(), watchData(SignalLine(7, 1))))
	}
	nudge("fail")
	start := time.Now()
	execTimeout = 300 * time.Millisecond
	got := nudge("sleep")
	execTimeout = shipped
	if got != retry {
		t.Errorf("timeout: Nudge = %+v, want %+v", got, retry)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("the timed-out run took %s; it was not stopped", took)
	}
	if got := nudge("fail"); got != retry {
		t.Errorf("second failure, after the timeout: Nudge = %+v, want %+v", got, retry)
	}
	if n := len(logs.records(t, "WARN")); n != 3 {
		t.Errorf("WARN lines = %d, want 3 (two failures and the timeout)", n)
	}
	if got := nudge("fail"); got != giveUp {
		t.Errorf("third failure: Nudge = %+v, want %+v", got, giveUp)
	}
}

func TestExecMissingProgram(t *testing.T) {
	door := loadDoor(t, lineFile("exec", "    argv: ['"+filepath.Join(t.TempDir(), "no-such-program")+"', '{{.Line}}']\n"))
	var out bytes.Buffer
	var logs logLines
	w := newWatch(t, door, &out, &logs)
	want := []Step{retry, retry, giveUp}
	for i, w0 := range want {
		if got := ok(t)(w.Nudge(t.Context(), watchData(SignalLine(7, 1)))); got != w0 {
			t.Errorf("run %d: Nudge = %+v, want %+v", i+1, got, w0)
		}
	}
}

func TestExecNewWatchTriesAgain(t *testing.T) {
	f := newFakeExec(t)
	f.mode(t, "fail")
	var out bytes.Buffer
	var logs logLines
	w := newWatch(t, f.door, &out, &logs)
	for range 3 {
		ok(t)(w.Nudge(t.Context(), watchData(SignalLine(7, 1))))
	}
	f.mode(t, "ok")
	again := newWatch(t, f.door, &out, &logs)
	if got := ok(t)(again.Nudge(t.Context(), watchData(SignalLine(8, 1)))); got != keepGoing {
		t.Errorf("a new watcher's Nudge = %+v, want %+v", got, keepGoing)
	}
}

func TestExecEnd(t *testing.T) {
	f := newFakeExec(t)
	f.mode(t, "ok")
	var out bytes.Buffer
	var logs logLines
	w := newWatch(t, f.door, &out, &logs)
	end := EndLine(EndDormant)
	if got := ok(t)(w.End(t.Context(), watchData(end))); !got.Exit || got.Code != 1 {
		t.Errorf("End = %+v, want an exit with code 1", got)
	}
	if runs := f.runs(t); len(runs) != 1 || runs[0][0] != end {
		t.Errorf("runs = %q, want one run carrying the end line", runs)
	}
}

func TestStdoutNudge(t *testing.T) {
	var out bytes.Buffer
	var logs logLines
	w := newWatch(t, loadDoor(t, lineFile("stdout", "")), &out, &logs)
	for _, seq := range []uint64{7, 8} {
		if got := ok(t)(w.Nudge(t.Context(), watchData(SignalLine(seq, 1)))); got != keepGoing {
			t.Errorf("Nudge %d = %+v, want %+v", seq, got, keepGoing)
		}
	}
	want := "[grelot:7] grelot (automated, not the user): 1 new; run grelot inbox\n" +
		"[grelot:8] grelot (automated, not the user): 1 new; run grelot inbox\n"
	if out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
}

func TestLineTemplate(t *testing.T) {
	var out bytes.Buffer
	var logs logLines
	w := newWatch(t, loadDoor(t, lineFile("stdout", "    line: '{\"wake\":{{json .Line}}}'\n")), &out, &logs)
	ok(t)(w.Nudge(t.Context(), watchData(SignalLine(7, 1))))
	want := `{"wake":"[grelot:7] grelot (automated, not the user): 1 new; run grelot inbox"}` + "\n"
	if out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
}

func TestExitNudge(t *testing.T) {
	var out bytes.Buffer
	var logs logLines
	w := newWatch(t, loadDoor(t, lineFile("exit", "")), &out, &logs)
	want := Step{Outcome: Accepted, Exit: true}
	if got := ok(t)(w.Nudge(t.Context(), watchData(SignalLine(7, 1)))); got != want {
		t.Errorf("Nudge = %+v, want %+v", got, want)
	}
	if out.String() != SignalLine(7, 1)+"\n" {
		t.Errorf("stdout = %q, want the signal line", out.String())
	}
}

func TestLineEnd(t *testing.T) {
	end := EndLine(EndEnded)
	cases := []struct {
		name, transport, extra, want string
		code                         int
	}{
		{"stdout default", "stdout", "", end + "\n", 1},
		{"exit default", "exit", "", end + "\n", 1},
		{"end follows line", "stdout", "    line: 'L {{.Line}}'\n", "L " + end + "\n", 1},
		{"end and end_exit", "exit", "    end: '{\"end\":{{json .Line}}}'\n    end_exit: 0\n", `{"end":"` + end + `"}` + "\n", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			var logs logLines
			w := newWatch(t, loadDoor(t, lineFile(c.transport, c.extra)), &out, &logs)
			want := Step{Outcome: Accepted, Exit: true, Code: c.code}
			if got := ok(t)(w.End(t.Context(), watchData(end))); got != want {
				t.Errorf("End = %+v, want %+v", got, want)
			}
			if out.String() != c.want {
				t.Errorf("stdout = %q, want %q", out.String(), c.want)
			}
		})
	}
}

func TestLineIdle(t *testing.T) {
	hold := "    hold: 45s\n    idle: '{\"outcome\":\"idle\"}'\n"
	cases := []struct {
		transport string
		want      Step
	}{
		{"exit", Step{Outcome: Accepted, Exit: true}},
		{"stdout", Step{Outcome: Accepted}},
	}
	for _, c := range cases {
		t.Run(c.transport, func(t *testing.T) {
			var out bytes.Buffer
			var logs logLines
			w := newWatch(t, loadDoor(t, lineFile(c.transport, hold)), &out, &logs)
			if got := ok(t)(w.Idle(t.Context(), watchData(""))); got != c.want {
				t.Errorf("Idle = %+v, want %+v", got, c.want)
			}
			if out.String() != `{"outcome":"idle"}`+"\n" {
				t.Errorf("stdout = %q, want the idle output", out.String())
			}
		})
	}
	t.Run("no hold", func(t *testing.T) {
		var out bytes.Buffer
		var logs logLines
		w := newWatch(t, loadDoor(t, lineFile("exit", "")), &out, &logs)
		if s, err := w.Idle(t.Context(), watchData("")); err == nil {
			t.Errorf("Idle = %+v with no error, want a refusal for a door with no hold", s)
		}
	})
	t.Run("exec", func(t *testing.T) {
		var out bytes.Buffer
		var logs logLines
		w := newWatch(t, newFakeExec(t).door, &out, &logs)
		if s, err := w.Idle(t.Context(), watchData("")); err == nil {
			t.Errorf("Idle = %+v with no error, want a refusal for an exec door", s)
		}
	})
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestStdoutWriteFails(t *testing.T) {
	var logs logLines
	w, err := NewWatch(loadDoor(t, lineFile("stdout", "")), failingWriter{}, logs.logger())
	if err != nil {
		t.Fatalf("NewWatch: %v", err)
	}
	if got := ok(t)(w.Nudge(t.Context(), watchData(SignalLine(7, 1)))); got != giveUp {
		t.Errorf("Nudge = %+v, want %+v", got, giveUp)
	}
}

func TestNewWatchRefusesHTTP(t *testing.T) {
	door := loadDoor(t, httpFile("https://receiver.example", ""))
	if w, err := NewWatch(door, &bytes.Buffer{}, nil); err == nil {
		t.Errorf("NewWatch = %+v, want a refusal for an http door", w)
	}
}
