package server

import (
	"io/fs"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/cplieger/grelot/internal/store"
)

const doorSecret = "door-secret-value-42"

// doorFile is a provider file named name with one http door whose
// Authorization header reads the secret secretName; match, when set, is
// the door's match line.
func doorFile(name, secretName, match string) string {
	return "name: " + name + "\ndoors:\n  - name: push\n    layer: 1\n    transport: http\n" +
		"    url: 'https://receiver.test/notify/{{path .Session}}'\n" +
		"    body: '{\"line\": {{json .Line}}, \"key\": {{json .DedupeKey}}}'\n" +
		"    headers: {Authorization: 'Bearer {{secret \"" + secretName + "\"}}'}\n" +
		"    dedupe_window: 10m\n    gone: [404, 410]\n" + match
}

// providers is a folder of the given name, content pairs, each name a
// file stem.
func providers(pairs ...string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for i := 0; i+1 < len(pairs); i += 2 {
		fsys[pairs[i]+".yaml"] = &fstest.MapFile{Data: []byte(pairs[i+1])}
	}
	return fsys
}

// withProviders serves the harness from fsys as its providers folder.
func withProviders(fsys fs.FS) func(*Options) {
	return func(o *Options) { o.Providers = fsys }
}

// attrs flattens one record's attributes.
func attrs(r slog.Record) map[string]string {
	out := map[string]string{}
	r.Attrs(func(a slog.Attr) bool {
		out[a.Key] = a.Value.String()
		return true
	})
	return out
}

func (h *harness) invalidLines() []map[string]string {
	var out []map[string]string
	for _, r := range h.logs.lines(slog.LevelError, msgProviderInvalid) {
		out = append(out, attrs(r))
	}
	return out
}

func TestDoors_aFileThatFailsToLoadIsSkippedWithOneLine(t *testing.T) {
	t.Setenv("SECRET_DOOR_TOKEN", doorSecret)
	h := newHarnessWith(t, store.Options{}, withProviders(providers(
		"broken", "name: broken\nno_such_key: 1\n",
		"testhost", doorFile("testhost", "door_token", ""),
	)))
	lines := h.invalidLines()
	if len(lines) != 1 {
		t.Fatalf("provider invalid lines = %v, want one", lines)
	}
	if lines[0]["provider"] != "broken" || lines[0]["problem"] == "" {
		t.Errorf("line = %v, want the broken file named with its problem", lines[0])
	}
	if got := h.srv.redact("x " + doorSecret); strings.Contains(got, doorSecret) {
		t.Errorf("redact = %q, want the loaded file's secret cut", got)
	}
}

func TestDoors_twoDoorsThatCanMatchOneIdentitySkipTheLaterFile(t *testing.T) {
	t.Setenv("SECRET_DOOR_TOKEN", doorSecret)
	h := newHarnessWith(t, store.Options{}, withProviders(providers(
		"alpha", doorFile("alpha", "door_token", "    match: {provider: testhost}\n"),
		"testhost", doorFile("testhost", "door_token", "    match: {host: host-a/app}\n"),
	)))
	lines := h.invalidLines()
	if len(lines) != 1 {
		t.Fatalf("provider invalid lines = %v, want one", lines)
	}
	if lines[0]["provider"] != "testhost" || !strings.Contains(lines[0]["problem"], "alpha") {
		t.Errorf("line = %v, want testhost skipped naming alpha", lines[0])
	}
}

func TestDoors_aBadOrUnsetSecretMakesTheFileInvalid(t *testing.T) {
	for _, tc := range []struct {
		name, secretName, value, wantProblem string
	}{
		{"a name outside the secret alphabet", "Door-Token", doorSecret, "secret"},
		{"an unset secret", "door_token", "", "SECRET_DOOR_TOKEN"},
		{"a value no header can carry", "door_token", doorSecret + "\r\nX-Injected: 1", "SECRET_DOOR_TOKEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SECRET_DOOR_TOKEN", tc.value)
			t.Setenv("SECRET_DOOR_TOKEN_FILE", "")
			h := newHarnessWith(t, store.Options{}, withProviders(providers(
				"testhost", doorFile("testhost", tc.secretName, ""),
			)))
			lines := h.invalidLines()
			if len(lines) != 1 {
				t.Fatalf("provider invalid lines = %v, want one", lines)
			}
			l := lines[0]
			if l["provider"] != "testhost" || l["key"] == "" || !strings.Contains(l["problem"], tc.wantProblem) {
				t.Errorf("line = %v, want testhost with the key named and %q in the problem", l, tc.wantProblem)
			}
			if strings.Contains(h.logs.text(), doorSecret) {
				t.Errorf("the log carries the secret's value:\n%s", h.logs.text())
			}
		})
	}
}

// unreadable is a folder every read of fails.
type unreadable struct{}

func (unreadable) Open(string) (fs.File, error) { return nil, fs.ErrPermission }

func TestDoors_anUnreadableFolderIsOneLineAndNoDoor(t *testing.T) {
	h := newHarnessWith(t, store.Options{}, withProviders(unreadable{}))
	lines := h.invalidLines()
	if len(lines) != 1 || lines[0]["key"] != "PROVIDERS_DIR" {
		t.Errorf("provider invalid lines = %v, want one naming PROVIDERS_DIR", lines)
	}
}

func TestDoors_aMissingFolderHoldsNoDoorAndLogsNothing(t *testing.T) {
	h := newHarnessWith(t, store.Options{}, func(o *Options) { o.Config.ProvidersDir = filepath.Join(t.TempDir(), "absent") })
	if lines := h.invalidLines(); len(lines) != 0 {
		t.Errorf("provider invalid lines = %v, want none", lines)
	}
}

func TestPost_aDoorSecretIsCutFromTheLogLine(t *testing.T) {
	t.Setenv("SECRET_DOOR_TOKEN", doorSecret)
	h := newHarnessWith(t, store.Options{}, withProviders(providers("testhost", doorFile("testhost", "door_token", ""))))
	h.mustRegister(sessA, "sender")
	h.mustRegister(sessB, "reader")
	r := h.as(sessA, http.MethodPost, "/v1/posts",
		postBody(map[string]any{"agent": "reader"}, "about "+doorSecret, "the key is "+doorSecret))
	if r.status != http.StatusCreated {
		t.Fatalf("post: %d %s", r.status, r.raw)
	}
	lines := h.logs.lines(slog.LevelInfo, "message posted")
	if len(lines) != 1 {
		t.Fatalf("message posted lines = %d, want 1", len(lines))
	}
	a := attrs(lines[0])
	for _, k := range []string{"subject", "body"} {
		if strings.Contains(a[k], doorSecret) || !strings.Contains(a[k], "[redacted]") {
			t.Errorf("%s = %q, want the door secret redacted", k, a[k])
		}
	}
}
