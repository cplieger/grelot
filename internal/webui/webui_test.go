package webui_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/cplieger/grelot/internal/webui"
)

const page = "<!doctype html><title>grelot</title>"

func built() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                    {Data: []byte(page)},
		"app-ABCDEFGH.js":               {Data: []byte("console.log(1)")},
		"app-ABCDEFGH.css":              {Data: []byte("p{}")},
		"assets/check-QRSTUVWX.svg":     {Data: []byte("<svg/>")},
		"licenses/ui/LICENSE":           {Data: []byte("Apache")},
		".gitkeep":                      {},
		"assets/.hidden":                {Data: []byte("x")},
		"assets/sub/inner-ABCDEFGH.txt": {Data: []byte("x")},
	}
}

type answer struct {
	header http.Header
	body   string
	status int
}

func get(t *testing.T, h http.Handler, method, target string) answer {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	return answer{status: rec.Code, header: rec.Result().Header, body: string(body)}
}

// The headers design D10 puts on every answer.
var wantHeaders = map[string]string{
	"Content-Security-Policy": "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; " +
		"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'",
	"X-Content-Type-Options": "nosniff",
	"Referrer-Policy":        "same-origin",
	"X-DNS-Prefetch-Control": "off",
}

func checkSecurityHeaders(t *testing.T, a answer) {
	t.Helper()
	for k, v := range wantHeaders {
		if got := a.header.Get(k); got != v {
			t.Errorf("%s: %q, want %q", k, got, v)
		}
	}
}

func TestHandler_everyAppAddressAnswersTheIndexRevalidated(t *testing.T) {
	h := webui.Handler(built())
	for _, target := range []string{
		"/", "/index.html", "/messages", "/messages/42?show=direct", "/agents/alpha", "/statistics?range=6h",
		"/settings/sign-in", "/sign-in?next=%2Fmessages", "/setup", "/recover",
	} {
		t.Run(strings.ReplaceAll(target, "/", "_"), func(t *testing.T) {
			a := get(t, h, http.MethodGet, target)
			if a.status != http.StatusOK || a.body != page {
				t.Fatalf("%d %q, want 200 and the index", a.status, a.body)
			}
			if got := a.header.Get("Cache-Control"); got != "no-cache" {
				t.Errorf("Cache-Control %q, want no-cache", got)
			}
			if got := a.header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
				t.Errorf("Content-Type %q, want text/html", got)
			}
			checkSecurityHeaders(t, a)
		})
	}
}

func TestHandler_aBundledFileIsCachedForAYear(t *testing.T) {
	h := webui.Handler(built())
	for target, typ := range map[string]string{
		"/app-ABCDEFGH.js":           "text/javascript",
		"/app-ABCDEFGH.css":          "text/css",
		"/assets/check-QRSTUVWX.svg": "image/svg+xml",
	} {
		t.Run(strings.ReplaceAll(target, "/", "_"), func(t *testing.T) {
			a := get(t, h, http.MethodGet, target)
			if a.status != http.StatusOK {
				t.Fatalf("%d, want 200", a.status)
			}
			if got := a.header.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
				t.Errorf("Cache-Control %q, want a year, immutable", got)
			}
			if got := a.header.Get("Content-Type"); !strings.HasPrefix(got, typ) {
				t.Errorf("Content-Type %q, want %s", got, typ)
			}
			checkSecurityHeaders(t, a)
		})
	}
}

func TestHandler_aNoticeIsRevalidated(t *testing.T) {
	a := get(t, webui.Handler(built()), http.MethodGet, "/licenses/ui/LICENSE")
	if a.status != http.StatusOK || a.body != "Apache" {
		t.Fatalf("%d %q, want 200 and the notice", a.status, a.body)
	}
	if got := a.header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control %q, want no-cache", got)
	}
}

func TestHandler_whatIsNoFileAndNoAppAddressIs404(t *testing.T) {
	h := webui.Handler(built())
	for _, target := range []string{
		"/missing.js", "/wp-login.php", "/assets", "/assets/", "/.gitkeep", "/assets/.hidden", "/licenses",
	} {
		t.Run(strings.ReplaceAll(target, "/", "_"), func(t *testing.T) {
			a := get(t, h, http.MethodGet, target)
			if a.status != http.StatusNotFound {
				t.Fatalf("%d %q, want 404", a.status, a.body)
			}
			if a.header.Get("Cache-Control") != "" {
				t.Errorf("Cache-Control %q on a 404, want none", a.header.Get("Cache-Control"))
			}
			checkSecurityHeaders(t, a)
		})
	}
}

func TestHandler_aFileInASubfolderIsServed(t *testing.T) {
	if a := get(t, webui.Handler(built()), http.MethodGet, "/assets/sub/inner-ABCDEFGH.txt"); a.status != http.StatusOK {
		t.Fatalf("%d, want 200", a.status)
	}
}

func TestHandler_headAnswersWithoutABody(t *testing.T) {
	a := get(t, webui.Handler(built()), http.MethodHead, "/messages")
	if a.status != http.StatusOK || a.body != "" {
		t.Fatalf("%d %q, want 200 with no body", a.status, a.body)
	}
}

func TestHandler_anotherMethodIs405(t *testing.T) {
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(m, func(t *testing.T) {
			a := get(t, webui.Handler(built()), m, "/messages")
			if a.status != http.StatusMethodNotAllowed {
				t.Fatalf("%d, want 405", a.status)
			}
			if got := a.header.Get("Allow"); got != "GET, HEAD" {
				t.Errorf("Allow %q, want GET, HEAD", got)
			}
			checkSecurityHeaders(t, a)
		})
	}
}

func TestHandler_anUnbuiltUIAnswers503AtEveryAppAddress(t *testing.T) {
	h := webui.Handler(fstest.MapFS{".gitkeep": {}})
	for _, target := range []string{"/", "/messages", "/sign-in"} {
		t.Run(strings.ReplaceAll(target, "/", "_"), func(t *testing.T) {
			a := get(t, h, http.MethodGet, target)
			if a.status != http.StatusServiceUnavailable {
				t.Fatalf("%d, want 503", a.status)
			}
			if !strings.Contains(a.body, "The web UI was not built: run go run ./cmd/bundle") {
				t.Errorf("body %q, want it to say how to build the UI", a.body)
			}
			if got := a.header.Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control %q, want no-store", got)
			}
			checkSecurityHeaders(t, a)
		})
	}
}

func TestFiles_isRootedAtTheBundleFolder(t *testing.T) {
	// A clone that has not bundled holds only the placeholder, which is
	// never served.
	a := get(t, webui.Handler(webui.Files()), http.MethodGet, "/.gitkeep")
	if a.status != http.StatusNotFound {
		t.Fatalf("/.gitkeep: %d, want 404", a.status)
	}
	if _, err := webui.Files().Open(".gitkeep"); err != nil {
		t.Fatalf("Files has no .gitkeep at its root: %v", err)
	}
}
