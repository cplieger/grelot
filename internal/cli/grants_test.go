//go:build !windows

package cli

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var reservationID = regexp.MustCompile(`reservation (\d+) (held|queued)`)

// mustReserve reserves and answers the reservation id it printed.
func (a *agent) mustReserve(args ...string) (id, out string) {
	a.t.Helper()
	out = a.must(append([]string{"reserve"}, args...)...)
	m := reservationID.FindStringSubmatch(out)
	if m == nil {
		a.t.Fatalf("reserve printed no reservation id:\n%s", out)
	}
	return m[1], out
}

func TestReserve_holdsQueuesAndHandsOff(t *testing.T) {
	ts := newServer(t)
	a, adir := registerAt(t, ts, "s-1", "ant-dev")
	b, _ := registerAt(t, ts, "s-2", "bee-dev")
	file := adir + "/engine.go"

	held, out := a.mustReserve(file, "--ttl", "1h", "--reason", "splitting the engine")
	if want := "grelot: reservation " + held + " held on " + file + " until 2026-11-05 10:00 UTC; renew with grelot renew " + held +
		" --ttl <d>, release with grelot release " + held + "\n"; !strings.Contains(out, want) {
		t.Errorf("reserve printed %q, want %q", out, want)
	}
	var sentBody struct {
		Path, Reason, TTL string
		Queue             *bool
	}
	if err := json.Unmarshal([]byte(ts.last(t, http.MethodPost, "/v1/reservations").Body), &sentBody); err != nil {
		t.Fatal(err)
	}
	if sentBody.Path != file || sentBody.Reason != "splitting the engine" || sentBody.TTL != "1h" || sentBody.Queue != nil {
		t.Errorf("body = %+v, want the path, reason and ttl, no queue", sentBody)
	}

	code, _, errOut := b.run("reserve", adir, "--ttl", "30m", "--reason", "x")
	if code != exitFailed || !strings.Contains(errOut, "ant-dev") {
		t.Errorf("a conflicting reserve: exit %d, stderr %q, want 1 naming the holder", code, errOut)
	}

	queued, out := b.mustReserve(adir, "--ttl", "30m", "--reason", "renaming", "--queue")
	if want := "grelot: reservation " + queued + " queued for " + adir + " behind @ant-dev (box · same machine, other environment) until 2026-11-05 10:00 UTC; the grant reaches you as mail; cancel with grelot release " + queued + label + "\n"; !strings.Contains(out, want) {
		t.Errorf("a queued reserve printed %q, want %q", out, want)
	}

	out = b.must("reservations", adir)
	for _, w := range []string{
		"reservation " + held + " · held · " + file + " · @ant-dev (box · same machine, other environment) · until 2026-11-05 10:00 UTC · reason: splitting the engine" + label + "\n",
		"reservation " + queued + " · queued · " + adir + " · @bee-dev (box · you) · for 30m0s once granted · reason: renaming\n",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("reservations printed %q, want %q", out, w)
		}
	}

	b.must("inbox")
	if out = a.must("release", held); !strings.Contains(out, "grelot: reservation "+held+" on "+file+" released\n") {
		t.Errorf("release printed %q", out)
	}
	if out = b.must("inbox"); !strings.Contains(out, "reservation_granted") {
		t.Errorf("the waiter's inbox printed %q, want the grant", out)
	}
	if out = b.must("release", queued); !strings.Contains(out, "grelot: reservation "+queued+" on "+adir+" released\n") {
		t.Errorf("releasing the granted path printed %q", out)
	}
}

func TestRelease_cancelsAQueueEntry(t *testing.T) {
	ts := newServer(t)
	a, adir := registerAt(t, ts, "s-1", "ant-dev")
	b, _ := registerAt(t, ts, "s-2", "bee-dev")
	a.mustReserve(adir, "--ttl", "1h", "--reason", "x")
	queued, _ := b.mustReserve(adir, "--ttl", "1h", "--reason", "y", "--queue")
	if out := b.must("release", queued); !strings.Contains(out, "grelot: reservation "+queued+" on "+adir+" cancelled\n") {
		t.Errorf("release of a queue entry printed %q, want cancelled", out)
	}
	if out := b.must("reservations"); strings.Contains(out, "reservation "+queued+" ") {
		t.Errorf("reservations still lists the cancelled entry: %q", out)
	}
	if code, _, errOut := b.run("release", "seven"); code != exitUsage || !strings.Contains(errOut, "grelot reservations") {
		t.Errorf("release seven: exit %d, stderr %q, want a usage error naming grelot reservations", code, errOut)
	}
}

var linkPattern = regexp.MustCompile(`grelot-link:\S+`)

func TestDelegate_printsTheLinkAndThePromptLine(t *testing.T) {
	isolateGit(t)
	ts := newServer(t)
	parent, pdir := registerAt(t, ts, "s-1", "ant-dev")
	registerAt(t, ts, "s-2", "bee-dev")
	clone := gitClone(t, filepath.Join(t.TempDir(), "src", "toolbelt"), toolbeltRemote)
	out := parent.must("delegate", "review", "--allow-thread", clone+"/internal", "--allow-thread", "announce",
		"--allow-agent", "@bee-dev", "--deny-thread", pdir+"/internal/legacy", "--ttl", "2h")

	link := linkPattern.FindString(out)
	if link == "" {
		t.Fatalf("delegate printed no link:\n%s", out)
	}
	want := "  Run `grelot help`, then register with `grelot register --delegate " + link +
		" --desc <your task>`, prefix every grelot command with `grelot as ant-dev/review`, never `--self`, and finish with `grelot as ant-dev/review bye`.\n"
	if !strings.Contains(out, want) {
		t.Errorf("delegate printed %q, want the prompt line %q", out, want)
	}
	if !strings.Contains(out, "add --self to your own commands") {
		t.Errorf("delegate printed %q, want the parent told to add --self", out)
	}
	var body struct {
		Label        string   `json:"label"`
		TTL          string   `json:"ttl"`
		AllowThreads []string `json:"allow_threads"`
		AllowAgents  []string `json:"allow_agents"`
		DenyThreads  []string `json:"deny_threads"`
	}
	if err := json.Unmarshal([]byte(ts.last(t, http.MethodPost, "/v1/delegates").Body), &body); err != nil {
		t.Fatal(err)
	}
	if body.Label != "review" || body.TTL != "2h" || strings.Join(body.AllowThreads, " ") != "/workspace/toolbelt/internal announce" ||
		strings.Join(body.AllowAgents, " ") != "bee-dev" || strings.Join(body.DenyThreads, " ") != pdir+"/internal/legacy" {
		t.Errorf("grant body = %+v, want the label, ttl and scope with names stripped of @", body)
	}
}

// withDelegate is a parent with one registered delegate, ant-dev/review,
// whose shell shares the parent's session, as a subagent's does.
func withDelegate(t *testing.T, ts *testServer) *agent {
	t.Helper()
	parent, _ := registerAt(t, ts, "s-1", "ant-dev")
	link := linkPattern.FindString(parent.must("delegate", "review", "--ttl", "1h"))
	parent.must("register", "--delegate", link, "--desc", "reviewing section 5")
	return parent
}

func TestDelegateGate_refusesACommandWithoutAs(t *testing.T) {
	ts := newServer(t)
	shell := withDelegate(t, ts)
	for _, args := range [][]string{{"inbox"}, {"bye"}} {
		code, _, errOut := shell.run(args...)
		if code != exitFailed || !strings.Contains(errOut, "this session has delegates (@ant-dev/review)") ||
			!strings.Contains(errOut, "grelot as <its handle>") || !strings.Contains(errOut, "--self") {
			t.Errorf("grelot %s from a session with delegates: exit %d, stderr %q, want the gate naming both fixes", args[0], code, errOut)
		}
	}
	if out := shell.must("--self", "me"); !strings.Contains(out, "@ant-dev · ") {
		t.Errorf("--self me printed %q, want the parent still registered after the refused bye", out)
	}
	if out := shell.must("as", "ant-dev/review", "inbox"); !strings.HasPrefix(out, "grelot: @ant-dev/review") {
		t.Errorf("as inbox printed %q, want the delegate's status line", out)
	}
	if req := ts.last(t, http.MethodGet, "/v1/inbox"); req.Header.Get("Grelot-As") != "ant-dev/review" || req.Header.Get("Grelot-Self") != "" {
		t.Errorf("as sent Grelot-As %q and Grelot-Self %q, want the handle alone", req.Header.Get("Grelot-As"), req.Header.Get("Grelot-Self"))
	}
}

func TestAs_refusesSelfAndAMissingCommand(t *testing.T) {
	ts := newServer(t)
	shell := withDelegate(t, ts)
	for _, args := range [][]string{
		{"--self", "as", "ant-dev/review", "inbox"},
		{"as", "ant-dev/review"},
		{"as", "ant-dev/review", "as", "x", "inbox"},
		{"as", "ant-dev/review", "nothing"},
	} {
		if code, _, errOut := shell.run(args...); code != exitUsage || !strings.Contains(errOut, "grelot as --help") {
			t.Errorf("grelot %s: exit %d, stderr %q, want a usage error naming grelot as --help", strings.Join(args, " "), code, errOut)
		}
	}
}

func TestDelegateRevoke_endsTheDelegate(t *testing.T) {
	ts := newServer(t)
	shell := withDelegate(t, ts)
	if out := shell.must("--self", "delegate", "revoke", "review"); !strings.Contains(out, "grelot: @ant-dev/review ended\n") {
		t.Errorf("revoke printed %q, want the delegate ended", out)
	}
	if out := shell.must("inbox"); !strings.HasPrefix(out, "grelot: @ant-dev · ") {
		t.Errorf("inbox after the revoke printed %q, want the gate lifted", out)
	}
	link := linkPattern.FindString(shell.must("delegate", "spare", "--ttl", "1h"))
	if link == "" {
		t.Fatal("no link")
	}
	if out := shell.must("--self", "delegate", "revoke", "spare"); !strings.Contains(out, "grelot: the unused link spare is revoked\n") {
		t.Errorf("revoking an unused link printed %q", out)
	}
	if code, _, errOut := shell.run("register", "--delegate", link, "--desc", "late"); code != exitFailed || errOut == "" {
		t.Errorf("registering with a revoked link: exit %d, stderr %q, want a refusal", code, errOut)
	}
}
