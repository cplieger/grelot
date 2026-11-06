//go:build !windows

package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/client"
	"github.com/cplieger/grelot/internal/where"
)

// toolbeltRemoteHash is the first 16 bytes of HMAC-SHA256 keyed "grelot
// remote" over the normalized remote, computed with openssl.
const (
	toolbeltRemote     = "git@github.com:cplieger/toolbelt.git"
	toolbeltRemoteHash = "3b9c6e0d9248a2015132854ae815633b"
)

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// registerAgent is an agent of session registered as toolbelt-dev on a
// clone of toolbelt, with its working directory in the clone.
func registerAgent(t *testing.T, ts *testServer, session string) (*agent, string) {
	t.Helper()
	isolateGit(t)
	a := newAgent(t, ts, session, "")
	clone := gitClone(t, filepath.Join(t.TempDir(), "home", "u", "src", "toolbelt"), toolbeltRemote)
	a.wd = clone
	a.must("register", "toolbelt-dev", "--paths", clone, "--desc", "testing the client", "--ttl", "2h")
	return a, clone
}

type sentRegister struct {
	Where    client.Where `json:"where"`
	Name     string       `json:"name"`
	Host     string       `json:"host"`
	OS       string       `json:"os"`
	Arch     string       `json:"arch"`
	Proc     string       `json:"proc"`
	TTL      string       `json:"ttl"`
	Paths    []string     `json:"paths"`
	TakeOver *bool        `json:"take_over"`
}

func TestRegister_printsTheStatusLineTheCardThenTheManual(t *testing.T) {
	isolateGit(t)
	ts := newServer(t)
	a := newAgent(t, ts, "s-1", "")
	clone := gitClone(t, filepath.Join(t.TempDir(), "home", "u", "src", "toolbelt"), toolbeltRemote)
	out := a.must("register", "toolbelt-dev", "--paths", clone+"/internal", "--desc", "testing the client", "--ttl", "2h")

	lines := strings.Split(out, "\n")
	if !strings.HasPrefix(lines[0], "grelot: @toolbelt-dev · 0 unread · wake: ") {
		t.Errorf("first line = %q, want the status line", lines[0])
	}
	card := "@toolbelt-dev · box · " + runtime.GOOS + "/" + runtime.GOARCH + " · provider alpha\n" +
		"  description: testing the client\n" +
		"  paths: /workspace/toolbelt/internal\n" +
		"  lease: 2h0m0s, until 2026-11-05 11:00 UTC\n"
	i, j := strings.Index(out, card), strings.Index(out, "# grelot help\n")
	if i < 0 || j < i {
		t.Errorf("output does not hold the card, then the manual:\n%s", out)
	}
}

func TestRegister_sendsTheIdentityAndWhereItRuns(t *testing.T) {
	isolateGit(t)
	ts := newServer(t)
	a := newAgent(t, ts, "s-1", "")
	clone := gitClone(t, filepath.Join(t.TempDir(), "toolbelt"), toolbeltRemote)
	a.must("register", "toolbelt-dev", "--paths", clone, "--desc", "testing the client", "--ttl", "2h")

	req := ts.last(t, http.MethodPost, "/v1/register")
	if got := req.Header.Get("Grelot-Session"); got != "alpha:s-1" {
		t.Errorf("Grelot-Session = %q, want alpha:s-1", got)
	}
	var body sentRegister
	if err := json.Unmarshal([]byte(req.Body), &body); err != nil {
		t.Fatal(err)
	}
	if body.Name != "toolbelt-dev" || body.Host != "box" || body.OS != runtime.GOOS || body.Arch != runtime.GOARCH || body.TTL != "2h" {
		t.Errorf("body = %+v, want the name, host label box, this OS and arch, and ttl 2h", body)
	}
	if len(body.Paths) != 1 || body.Paths[0] != "/workspace/toolbelt" {
		t.Errorf("paths = %q, want the clone's canonical root /workspace/toolbelt", body.Paths)
	}
	if body.Proc != "200:20" {
		t.Errorf("proc = %q, want the alphacli ancestor's pid and start, 200:20", body.Proc)
	}
	if body.TakeOver != nil {
		t.Errorf("take_over = %v, want it absent without --take-over", *body.TakeOver)
	}
	if body.Where.Machine != testMachine || !hex32.MatchString(body.Where.Environment) {
		t.Errorf("where = %+v, want the machine id and a 32-hex environment id", body.Where)
	}
	if len(body.Where.Checkouts) != 1 {
		t.Fatalf("checkouts = %+v, want one for toolbelt", body.Where.Checkouts)
	}
	c := body.Where.Checkouts[0]
	if c.Repo != "/workspace/toolbelt" || c.Remote != toolbeltRemoteHash || !hex32.MatchString(c.ID) {
		t.Errorf("checkout = %+v, want repo /workspace/toolbelt, remote hash %s and a 32-hex id", c, toolbeltRemoteHash)
	}
}

func TestRegister_aPathOutsideAnyCloneSendsNoCheckout(t *testing.T) {
	ts := newServer(t)
	a := newAgent(t, ts, "s-1", "")
	dir := filepath.Join(t.TempDir(), "notes")
	a.must("register", "notes-dev", "--paths", dir, "--desc", "notes", "--ttl", "1h")
	var body sentRegister
	if err := json.Unmarshal([]byte(ts.last(t, http.MethodPost, "/v1/register").Body), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Paths) != 1 || body.Paths[0] != filepath.ToSlash(dir) || len(body.Where.Checkouts) != 0 {
		t.Errorf("paths = %q, checkouts = %+v, want the local path and no checkout", body.Paths, body.Where.Checkouts)
	}
}

func TestRegister_withoutTTLNamesTheFlag(t *testing.T) {
	ts := newServer(t)
	a := newAgent(t, ts, "s-1", "")
	code, _, errOut := a.run("register", "tool-dev", "--paths", "/workspace/tool", "--desc", "x")
	if code != 1 || !strings.Contains(errOut, "--ttl") {
		t.Errorf("exit %d, stderr %q; want 1 and a refusal naming --ttl", code, errOut)
	}
}

func TestRegister_againReturnsTheIdentityUnchanged(t *testing.T) {
	ts := newServer(t)
	a, clone := registerAgent(t, ts, "s-1")
	ts.clock.advance(10 * time.Minute)
	out := a.must("register", "toolbelt-dev", "--paths", clone, "--desc", "testing the client", "--ttl", "6h")
	if !strings.Contains(out, "already has the identity @toolbelt-dev") || !strings.Contains(out, "grelot renew --ttl") {
		t.Errorf("output = %q, want the existing identity named, with grelot renew", out)
	}
	if !strings.Contains(out, "lease: 2h0m0s, until 2026-11-05 11:00 UTC") {
		t.Errorf("output = %q, want the first lease unchanged", out)
	}
}

func TestRegister_aHeldName(t *testing.T) {
	ts := newServer(t)
	a, clone := registerAgent(t, ts, "s-1")

	t.Run("another session is refused with the holder", func(t *testing.T) {
		b := newAgent(t, ts, "s-2", "")
		b.env.ConfigDir = a.env.ConfigDir
		code, _, errOut := b.run("register", "toolbelt-dev", "--paths", clone, "--desc", "x", "--ttl", "1h")
		if code != 1 || !strings.Contains(errOut, "held by a live identity") || !strings.Contains(errOut, "--take-over") {
			t.Errorf("exit %d, stderr %q; want 1 naming the holder and --take-over", code, errOut)
		}
	})
	t.Run("a take-over from another environment is refused", func(t *testing.T) {
		c := newAgent(t, ts, "s-3", "")
		code, _, errOut := c.run("register", "toolbelt-dev", "--paths", clone, "--desc", "x", "--ttl", "1h", "--take-over")
		if code != 1 || !strings.Contains(errOut, "is not in this environment") {
			t.Errorf("exit %d, stderr %q; want 1 saying the identity is not in this environment", code, errOut)
		}
	})
	t.Run("a take-over in the same environment moves it", func(t *testing.T) {
		d := newAgent(t, ts, "s-4", "")
		d.env.ConfigDir = a.env.ConfigDir
		out := d.must("register", "toolbelt-dev", "--paths", clone, "--desc", "taken over", "--ttl", "1h", "--take-over")
		if !strings.Contains(out, "description: taken over") {
			t.Errorf("output = %q, want the moved identity's card", out)
		}
	})
}

func TestRegister_noEnvironmentNamesTheFile(t *testing.T) {
	ts := newServer(t)
	a := newAgent(t, ts, "s-1", "")
	file := where.EnvironmentFile(a.env.ConfigDir)
	if err := os.MkdirAll(file, 0o700); err != nil {
		t.Fatal(err)
	}
	out := a.must("register", "tool-dev", "--paths", "/workspace/tool", "--desc", "x", "--ttl", "1h")
	want := "grelot: this session has no environment id (" + file + " could not be read or created): another session cannot hand this identity over"
	if !strings.Contains(out, want) {
		t.Errorf("output = %q, want %q", out, want)
	}
}

func TestRegister_aDelegate(t *testing.T) {
	ts := newServer(t)
	a, _ := registerAgent(t, ts, "s-1")
	var grant struct {
		Link string `json:"link"`
	}
	ts.call(t, "alpha:s-1", false, &client.Request{
		Method: http.MethodPost, Path: "/v1/delegates",
		Body: map[string]any{"label": "review", "ttl": "1h"},
	}, &grant)

	out := a.must("register", "--delegate", grant.Link, "--desc", "reviewing the diff")
	if !strings.Contains(out, "@toolbelt-dev/review · ") || !strings.Contains(out, "description: reviewing the diff") {
		t.Errorf("output = %q, want the delegate's card", out)
	}
	if !strings.Contains(out, "# grelot help\n") {
		t.Errorf("output = %q, want the manual after the card", out)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(ts.last(t, http.MethodPost, "/v1/register").Body), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["where"]; ok || body["delegate"] != grant.Link {
		t.Errorf("body = %v, want the link and no where, which a delegate takes from its parent", body)
	}

	t.Run("the parent's command without --self is refused by the gate", func(t *testing.T) {
		code, _, errOut := a.run("me")
		if code != 1 || !strings.Contains(errOut, "--self") || !strings.Contains(errOut, "grelot as") {
			t.Errorf("exit %d, stderr %q; want 1 naming grelot as and --self", code, errOut)
		}
	})
	t.Run("--self passes the gate", func(t *testing.T) {
		if out := a.must("--self", "me"); !strings.Contains(out, "delegate @toolbelt-dev/review: reviewing the diff") {
			t.Errorf("output = %q, want the delegate listed", out)
		}
	})
}

func TestRegister_aDelegateTakesNoIdentityFlags(t *testing.T) {
	a := newAgent(t, newServer(t), "s-1", "")
	code, _, errOut := a.run("register", "--delegate", "grelot-link:x", "--desc", "x", "--ttl", "1h")
	if code != 2 || !strings.Contains(errOut, "--delegate <link> --desc <your task>") {
		t.Errorf("exit %d, stderr %q; want 2 naming the delegate form", code, errOut)
	}
}

func TestRegister_refreshesAnUnconfirmedRootFirst(t *testing.T) {
	isolateGit(t)
	ts := newServer(t)
	a := newAgent(t, ts, "s-1", "")
	client.SaveFacts(a.env.ConfigDir, client.Facts{PathRoot: "/srv/old"})
	clone := gitClone(t, filepath.Join(t.TempDir(), "toolbelt"), toolbeltRemote)
	a.must("register", "toolbelt-dev", "--paths", clone, "--desc", "x", "--ttl", "1h")
	var body sentRegister
	if err := json.Unmarshal([]byte(ts.last(t, http.MethodPost, "/v1/register").Body), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Paths) != 1 || body.Paths[0] != "/workspace/toolbelt" {
		t.Errorf("paths = %q, want the server's root /workspace, not the cached /srv/old", body.Paths)
	}
}

func TestRevive(t *testing.T) {
	ts := newServer(t)
	a, clone := registerAgent(t, ts, "s-1")
	ts.clock.advance(3 * time.Hour)

	t.Run("the dormant session's me shows the revive offer", func(t *testing.T) {
		out := a.must("me")
		if !strings.Contains(out, "grelot revive --ttl <d>") || !strings.Contains(out, "dormant since 2026-11-05 11:00 UTC") {
			t.Errorf("output = %q, want the revive offer and the dormant card", out)
		}
	})
	t.Run("another environment cannot revive it by name", func(t *testing.T) {
		c := newAgent(t, ts, "s-3", "")
		code, _, errOut := c.run("revive", "toolbelt-dev", "--ttl", "1h")
		if code != 1 || !strings.Contains(errOut, "no dormant identity toolbelt-dev in this environment") {
			t.Errorf("exit %d, stderr %q; want 1 with the 404 for another environment", code, errOut)
		}
	})
	t.Run("another session of its environment revives it by name", func(t *testing.T) {
		b := newAgent(t, ts, "s-2", "")
		b.env.ConfigDir, b.wd = a.env.ConfigDir, clone
		out := b.must("revive", "toolbelt-dev", "--ttl", "1h")
		if !strings.Contains(out, "lease: 1h0m0s, until 2026-11-05 13:00 UTC") || !strings.Contains(out, "# grelot help\n") {
			t.Errorf("output = %q, want the card with the new lease, then the manual", out)
		}
		var body struct {
			Where client.Where `json:"where"`
			Name  string       `json:"name"`
			Proc  string       `json:"proc"`
		}
		if err := json.Unmarshal([]byte(ts.last(t, http.MethodPost, "/v1/revive").Body), &body); err != nil {
			t.Fatal(err)
		}
		if body.Name != "toolbelt-dev" || body.Proc != "200:20" || len(body.Where.Checkouts) != 1 ||
			body.Where.Checkouts[0].Repo != "/workspace/toolbelt" {
			t.Errorf("body = %+v, want the name, proc and the working directory's checkout", body)
		}
	})
}

func TestRevive_ownSessionCountsUnread(t *testing.T) {
	ts := newServer(t)
	a, _ := registerAgent(t, ts, "s-1")
	newAgent(t, ts, "s-2", "").must("register", "docs-dev", "--paths", "/workspace/docs", "--desc", "docs", "--ttl", "6h")
	ts.call(t, "alpha:s-2", false, &client.Request{Method: http.MethodPost, Path: "/v1/posts", Body: map[string]any{
		"to": map[string]string{"agent": "toolbelt-dev"}, "subject": "Question", "body": "hello",
		"identity": map[string]any{"name": noChange, "paths": []string{noChange}, "description": noChange},
	}}, nil)
	ts.clock.advance(3 * time.Hour)
	out := a.must("revive", "--ttl", "1h")
	if !strings.Contains(out, "grelot: 1 unread: run grelot inbox\n") || !strings.Contains(out, "lease: 1h0m0s") {
		t.Errorf("output = %q, want the unread count and the revived card", out)
	}
}

func TestMe(t *testing.T) {
	ts := newServer(t)
	a, clone := registerAgent(t, ts, "s-1")
	out := a.must("me")
	file := where.EnvironmentFile(a.env.ConfigDir)
	if !strings.Contains(out, "@toolbelt-dev · box") || !strings.Contains(out, "machine: known (dmi) · environment: known, keyed by "+file) {
		t.Errorf("output = %q, want the card and what is known of the machine and environment", out)
	}
	key, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	envID := where.Environment(t.Context(), file, false)
	checkoutKey, err := os.ReadFile(where.CheckoutFile(filepath.Join(clone, ".git")))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{strings.TrimSpace(string(key)), envID, strings.TrimSpace(string(checkoutKey)), testMachine[4:]} {
		if strings.Contains(out, secret) {
			t.Errorf("grelot me printed %q, a key or an id", secret)
		}
	}
}

func TestMe_unregistered(t *testing.T) {
	a := newAgent(t, newServer(t), "s-1", "")
	code, out, _ := a.run("me")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 2 || !strings.Contains(lines[0], "grelot register <name>") ||
		!strings.Contains(lines[1], "then read what it prints") {
		t.Errorf("exit %d, output %q; want 0, the register command, then the line to read what it prints", code, out)
	}
}

func TestUnregistered_aCommandAnswersTheRegisterCommand(t *testing.T) {
	a := newAgent(t, newServer(t), "s-1", "")
	for _, args := range [][]string{{"set", "--desc", "x"}, {"renew", "--ttl", "1h"}, {"bye"}} {
		t.Run(args[0], func(t *testing.T) {
			code, out, errOut := a.run(args...)
			want := "grelot: this session has no identity: run grelot register <name> --paths <path> --desc <what and why> --ttl <d>, then read what it prints\n"
			if code != 1 || out != "" || errOut != want {
				t.Errorf("exit %d, stdout %q, stderr %q; want 1, nothing, and %q", code, out, errOut, want)
			}
		})
	}
}

func TestSet(t *testing.T) {
	ts := newServer(t)
	a, clone := registerAgent(t, ts, "s-1")

	t.Run("a description", func(t *testing.T) {
		out := a.must("set", "--desc", "now reviewing")
		if !strings.Contains(out, "description: now reviewing") {
			t.Errorf("output = %q, want the new description on the card", out)
		}
		if strings.Contains(ts.last(t, http.MethodPatch, "/v1/me").Body, `"where"`) {
			t.Error("a description change sent where; only new paths or a renewal carry it")
		}
	})
	t.Run("new paths carry their checkouts", func(t *testing.T) {
		out := a.must("set", "--paths", clone+"/internal,"+clone+"/cmd")
		if !strings.Contains(out, "paths: /workspace/toolbelt/internal, /workspace/toolbelt/cmd") {
			t.Errorf("output = %q, want both canonical paths", out)
		}
		var body patchBody
		if err := json.Unmarshal([]byte(ts.last(t, http.MethodPatch, "/v1/me").Body), &body); err != nil {
			t.Fatal(err)
		}
		if body.Where == nil || len(body.Where.Checkouts) != 1 || body.Where.Checkouts[0].Repo != "/workspace/toolbelt" {
			t.Errorf("where = %+v, want one checkout for toolbelt", body.Where)
		}
	})
	t.Run("a ttl renews with the working directory's checkout", func(t *testing.T) {
		out := a.must("set", "--ttl", "5h")
		if !strings.Contains(out, "lease: 5h0m0s, until 2026-11-05 14:00 UTC") {
			t.Errorf("output = %q, want the renewed lease", out)
		}
		var body patchBody
		if err := json.Unmarshal([]byte(ts.last(t, http.MethodPatch, "/v1/me").Body), &body); err != nil {
			t.Fatal(err)
		}
		if body.Where == nil || len(body.Where.Checkouts) != 1 {
			t.Errorf("where = %+v, want the working directory's checkout", body.Where)
		}
	})
	t.Run("nothing to set is refused with the flags", func(t *testing.T) {
		code, _, errOut := a.run("set")
		if code != 1 || !strings.Contains(errOut, "--desc") {
			t.Errorf("exit %d, stderr %q; want 1 naming the flags", code, errOut)
		}
	})
}

func TestRenew(t *testing.T) {
	ts := newServer(t)
	a, _ := registerAgent(t, ts, "s-1")
	ts.clock.advance(time.Hour)

	t.Run("the identity", func(t *testing.T) {
		out := a.must("renew", "--ttl", "4h")
		if !strings.Contains(out, "grelot: @toolbelt-dev renewed for 4h0m0s, until 2026-11-05 14:00 UTC") {
			t.Errorf("output = %q, want the lease from now", out)
		}
		var body patchBody
		if err := json.Unmarshal([]byte(ts.last(t, http.MethodPatch, "/v1/me").Body), &body); err != nil {
			t.Fatal(err)
		}
		if body.TTL != "4h" || body.Where == nil || len(body.Where.Checkouts) != 1 || body.Where.Checkouts[0].Repo != "/workspace/toolbelt" {
			t.Errorf("body = %+v, want ttl 4h and the working directory's checkout", body)
		}
	})
	t.Run("a reservation", func(t *testing.T) {
		var got client.ReservationAnswer
		ts.call(t, "alpha:s-1", false, &client.Request{
			Method: http.MethodPost, Path: "/v1/reservations",
			Body: map[string]any{"path": "/workspace/toolbelt/engine.go", "reason": "editing", "ttl": "30m"},
		}, &got)
		out := a.must("renew", strings.TrimSpace(jsonNumber(got.Reservation.ID)), "--ttl", "45m")
		if !strings.Contains(out, "on /workspace/toolbelt/engine.go renewed for 45m0s, until 2026-11-05 10:45 UTC") {
			t.Errorf("output = %q, want the reservation's new expiry", out)
		}
	})
	t.Run("a word that is no id", func(t *testing.T) {
		code, _, errOut := a.run("renew", "abc", "--ttl", "1h")
		if code != 2 || !strings.Contains(errOut, "grelot reservations") {
			t.Errorf("exit %d, stderr %q; want 2 naming grelot reservations", code, errOut)
		}
	})
}

func jsonNumber(n uint64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestBye(t *testing.T) {
	ts := newServer(t)
	a, _ := registerAgent(t, ts, "s-1")
	// The answer is the retirement alone: the no-identity status line that
	// follows a retirement would ask the agent that just left to register.
	if out := a.must("bye"); out != "grelot: identity retired: its reservations went to their queues and its delegates ended\n" {
		t.Errorf("output = %q, want only the retirement line", out)
	}
	if out := a.must("me"); !strings.Contains(out, "this session has no identity") {
		t.Errorf("me after bye = %q, want no identity", out)
	}
}
