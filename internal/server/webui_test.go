//go:build !windows

package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/cplieger/grelot/internal/store"
)

const testPage = "<!doctype html><title>grelot test page</title>"

func withUI(o *Options) {
	o.UI = fstest.MapFS{"index.html": {Data: []byte(testPage)}, "app-ABCDEFGH.js": {Data: []byte("1")}}
}

func pathOf(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	return u.Path
}

func TestHandler_pagesAndFilesOutsideTheAPIAreTheWebUIs(t *testing.T) {
	h := newHarnessWith(t, store.Options{}, withAuth(testAuth(t)), withUI)
	setup, err := h.srv.auth.SetupToken(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/messages/7", "/sign-in", pathOf(t, setup.Link)} {
		t.Run(strings.ReplaceAll(path, "/", "_"), func(t *testing.T) {
			r := h.request(http.MethodGet, path, nil, nil)
			if r.status != http.StatusOK || r.raw != testPage {
				t.Fatalf("%d %q, want the UI's page", r.status, r.raw)
			}
			if !strings.Contains(r.header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
				t.Errorf("Content-Security-Policy %q, want the UI's", r.header.Get("Content-Security-Policy"))
			}
		})
	}
	if r := h.request(http.MethodGet, "/app-ABCDEFGH.js", nil, nil); r.status != http.StatusOK || r.raw != "1" {
		t.Errorf("a bundled file: %d %q, want 200", r.status, r.raw)
	}
}

func TestHandler_theRecoveryLinkOpensAUIPage(t *testing.T) {
	h := newSignedIn(t, withUI)
	tok, err := h.srv.auth.Recovery(t.Context(), ownerName)
	if err != nil {
		t.Fatal(err)
	}
	if r := h.request(http.MethodGet, pathOf(t, tok.Link), nil, nil); r.status != http.StatusOK || r.raw != testPage {
		t.Fatalf("%s: %d %q, want the UI's page", tok.Link, r.status, r.raw)
	}
}

func TestHandler_theAPIKeepsItsOwnAnswerForAnUnknownRoute(t *testing.T) {
	h := newSignedIn(t, withUI)
	for _, path := range []string{"/v1/nope", "/api/nope"} {
		t.Run(strings.ReplaceAll(path, "/", "_"), func(t *testing.T) {
			r := h.request(http.MethodGet, path, nil, nil)
			if r.status != http.StatusNotFound || r.str("code") != "auth_not_found" || r.header.Get("Content-Security-Policy") != "" {
				t.Fatalf("%d %q, want the API's JSON 404", r.status, r.raw)
			}
		})
	}
}
