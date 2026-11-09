package providers_test

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/cplieger/grelot/internal/provider"
	"github.com/cplieger/grelot/internal/transport"
	"github.com/cplieger/grelot/providers"
)

// marotteBase is where the shipped door posts: marotte's service on a
// compose network the server's container joins.
const marotteBase = "http://marotte:9847"

const marotteKey = "test-notify-key"

// notifyBody is marotte's notify request, {text, source, dedupe_key}.
type notifyBody struct {
	Text      string `json:"text"`
	Source    string `json:"source"`
	DedupeKey string `json:"dedupe_key"`
}

// marotteNudge is a nudge for a session id holding the URL delimiters.
func marotteNudge() *provider.Data {
	return &provider.Data{Line: transport.SignalLine(3, 2), Seq: 3, Session: "sess/a?b%c", Identity: "marotte-dev", DedupeKey: "id1:3:1"}
}

// withMarotteKey sets the door's secret as the server reads it.
func withMarotteKey(t *testing.T) {
	t.Helper()
	t.Setenv("SECRET_MAROTTE_TOKEN", marotteKey)
	unsetEnv(t, "SECRET_MAROTTE_TOKEN_FILE")
}

func unsetEnv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
}

// loadMarotte loads fsys as the server's providers folder and returns its
// one door, which must come from marotte.yaml.
func loadMarotte(t *testing.T, fsys fs.FS) (*provider.ServerDoors, *provider.Door) {
	t.Helper()
	doors, skipped, err := provider.LoadServerDir(fsys)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("LoadServerDir: %v, skipped %v", err, skipped)
	}
	all := doors.All()
	if len(all) != 1 || all[0].File != "marotte" {
		t.Fatalf("server doors %+v, want marotte's alone", all)
	}
	return doors, all[0].Door
}

func TestMarotte_file(t *testing.T) {
	p := shipped(t, "marotte")
	if p.Evidence != provider.EvidenceMeasured || len(p.Detect.Env) != 0 || p.Session.Env != "" || p.Session.Stdin != "" {
		t.Errorf("evidence %q, detect %v, session %+v; want measured, no detect and no session", p.Evidence, p.Detect.Env, p.Session)
	}
	if p.Hooks != (provider.Hooks{}) {
		t.Errorf("hooks %+v, want none: a marotte chat runs kiro-cli's", p.Hooks)
	}
	if len(p.Doors) != 1 {
		t.Fatalf("doors %+v, want the notify door alone", p.Doors)
	}
	d := &p.Doors[0]
	if d.Name != "notify" || d.Layer != 1 || d.Transport != provider.TransportHTTP {
		t.Errorf("door %s, layer %d, transport %s; want the layer 1 http door notify", d.Name, d.Layer, d.Transport)
	}
	if d.DedupeWindow != 10*time.Minute || !slices.Equal(d.Gone, []int{404}) {
		t.Errorf("dedupe_window %v, gone %v; want marotte's fixed 10 min window and 404 as gone", d.DedupeWindow, d.Gone)
	}
}

// The server renders marotte's notify request: the session escaped as one
// path segment, the bearer key from SECRET_MAROTTE_TOKEN, and the body
// {text, source, dedupe_key} as JSON.
func TestMarotteNotify_request(t *testing.T) {
	withMarotteKey(t)
	doors, door := loadMarotte(t, providers.FS)
	if got := doors.Secrets(); !slices.Equal(got, []string{marotteKey}) {
		t.Errorf("secrets %v, want the key alone, so the server redacts it", got)
	}
	r, err := door.RenderRequest(marotteNudge())
	if err != nil {
		t.Fatal(err)
	}
	if want := marotteBase + "/api/sessions/sess%2Fa%3Fb%25c/notify"; r.URL != want {
		t.Errorf("url %q, want %q", r.URL, want)
	}
	if want := map[string]string{"Authorization": "Bearer " + marotteKey, "Content-Type": "application/json"}; !maps.Equal(r.Header, want) {
		t.Errorf("headers %v, want %v", r.Header, want)
	}
	got := decodeNotify(t, r.Body)
	if want := (notifyBody{Text: transport.SignalLine(3, 2), Source: "grelot", DedupeKey: "id1:3:1"}); got != want {
		t.Errorf("body %+v, want %+v", got, want)
	}
}

// Each of marotte's answers comes to the outcome the server acts on: both
// 200 forms accepted unread, 404 gone, 503 a retry after its Retry-After,
// and an auth refusal a give up.
func TestMarotteNotify_outcomes(t *testing.T) {
	withMarotteKey(t)
	cases := []struct {
		name       string
		status     int
		body       string
		retryAfter string
		want       transport.Outcome
		wantAfter  time.Duration
	}{
		{name: "delivered", status: 200, body: `{"delivered":"steer"}`, want: transport.Accepted},
		{name: "duplicate", status: 200, body: `{"duplicate":true}`, want: transport.Accepted},
		{name: "unknown session", status: 404, want: transport.Gone},
		{name: "busy", status: 503, retryAfter: "30", want: transport.RetryLater, wantAfter: 30 * time.Second},
		{name: "key refused", status: 401, want: transport.GiveUp},
		{name: "scope missing", status: 403, want: transport.GiveUp},
	}
	for _, c := range cases {
		t.Run(strings.ReplaceAll(c.name, " ", "_"), func(t *testing.T) {
			type seen struct {
				method, path, auth, ctype, body string
			}
			got := make(chan seen, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				got <- seen{r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(b)}
				if c.retryAfter != "" {
					w.Header().Set("Retry-After", c.retryAfter)
				}
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, c.body)
			}))
			t.Cleanup(srv.Close)
			_, door := loadMarotte(t, marotteAt(t, srv.URL))

			res, err := transport.NewHTTP().Send(t.Context(), door, marotteNudge())
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != c.want || res.Status != c.status || res.RetryAfter != c.wantAfter {
				t.Errorf("result %+v, want outcome %d, status %d, retry after %v", res, c.want, c.status, c.wantAfter)
			}
			s := <-got
			if s.method != http.MethodPost || s.path != "/api/sessions/sess%2Fa%3Fb%25c/notify" || s.auth != "Bearer "+marotteKey || s.ctype != "application/json" {
				t.Errorf("request %s %s, auth %q, content type %q; want POST to the escaped session with the bearer key and JSON", s.method, s.path, s.auth, s.ctype)
			}
			decodeNotify(t, s.body)
		})
	}
}

// The door serves kiro-cli identities on its host label alone: a kiro-cli
// session on another host, and every other provider, keeps its watcher.
func TestMarotteNotify_match(t *testing.T) {
	withMarotteKey(t)
	doors, door := loadMarotte(t, providers.FS)
	label := door.Match.Host
	if door.Match.Provider != "kiro-cli" || label == "" {
		t.Fatalf("match %+v, want kiro-cli on one host label", door.Match)
	}
	if d, ok := doors.For("kiro-cli", label); !ok || d.File != "marotte" {
		t.Errorf("For(kiro-cli, %s) = %+v, %v; want marotte's door", label, d, ok)
	}
	for _, c := range []struct{ provider, host string }{
		{"kiro-cli", "box"},
		{"kiro-cli", ""},
		{"marotte", label},
		{"kiro-ide", label},
		{"claude-code", label},
		{"codex", label},
	} {
		if d, ok := doors.For(c.provider, c.host); ok {
			t.Errorf("For(%s, %q) = %+v, want no door", c.provider, c.host, d)
		}
	}
}

// Without its key the file is skipped as invalid, naming the variable, and
// the server pushes to nobody.
func TestMarotteNotify_withoutItsKey(t *testing.T) {
	unsetEnv(t, "SECRET_MAROTTE_TOKEN")
	unsetEnv(t, "SECRET_MAROTTE_TOKEN_FILE")
	doors, skipped, err := provider.LoadServerDir(providers.FS)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0].Provider != "marotte" || !strings.Contains(skipped[0].Error(), "SECRET_MAROTTE_TOKEN is not set") {
		t.Errorf("skipped %v, want marotte's file naming SECRET_MAROTTE_TOKEN", skipped)
	}
	if all := doors.All(); len(all) != 0 {
		t.Errorf("doors %+v, want none", all)
	}
}

// marotteAt is a providers folder holding the shipped marotte.yaml with its
// door posting to base instead.
func marotteAt(t *testing.T, base string) fs.FS {
	t.Helper()
	data, err := fs.ReadFile(providers.FS, "marotte.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(data, []byte(marotteBase+"/")); n != 1 {
		t.Fatalf("marotte.yaml names %s %d times, want once, in the door's url", marotteBase, n)
	}
	return fstest.MapFS{"marotte.yaml": {Data: bytes.Replace(data, []byte(marotteBase), []byte(base), 1)}}
}

// decodeNotify parses a notify body, failing on any key marotte's verb
// does not take.
func decodeNotify(t *testing.T, body string) notifyBody {
	t.Helper()
	var b notifyBody
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	return b
}
