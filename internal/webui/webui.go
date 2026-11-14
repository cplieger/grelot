// Package webui serves the browser UI: the files cmd/bundle builds, the
// page for each of the app's addresses, and the headers every answer
// carries.
package webui

import (
	"bytes"
	"io"
	"io/fs"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"
)

// Index is the page every app address answers with.
const Index = "index.html"

// licenseDir holds the notices of the libraries the bundle ships, under
// their own names, so they are revalidated rather than kept for a year.
const licenseDir = "licenses"

// notBuilt is the answer to an app address while the bundle is missing.
const notBuilt = "The web UI was not built: run go run ./cmd/bundle"

// pages are the first path segments the browser's router answers; any
// other path that is no file is a 404.
var pages = []string{"messages", "agents", "statistics", "settings", "sign-in", "setup", "recover"}

// securityHeaders go on every answer. X-DNS-Prefetch-Control stops a
// browser resolving the hosts of agents' links before any click
// (@cplieger/ui docs/content/markdown.md, safety item 9).
var securityHeaders = map[string]string{
	"Content-Security-Policy": "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; " +
		"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'",
	"X-Content-Type-Options": "nosniff",
	"Referrer-Policy":        "same-origin",
	"X-DNS-Prefetch-Control": "off",
}

// Handler serves files, a built UI as Files gives it. Every file but
// Index and the notices is content-addressed by cmd/bundle, so it is
// cached for a year; Index is revalidated on every load. An app address
// answers 503 while files holds no Index.
func Handler(files fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range securityHeaders {
			w.Header().Set(k, v)
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "the web UI answers GET and HEAD", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		switch {
		case isPage(name):
			servePage(w, r, files)
		case servable(name):
			serveFile(w, r, files, name)
		default:
			http.NotFound(w, r)
		}
	})
}

func isPage(name string) bool {
	if name == "" || name == "." || name == Index {
		return true
	}
	first, _, _ := strings.Cut(name, "/")
	return slices.Contains(pages, first)
}

// servable refuses a dot-file (static/.gitkeep) and anything fs.FS
// cannot name.
func servable(name string) bool {
	if !fs.ValidPath(name) {
		return false
	}
	for part := range strings.SplitSeq(name, "/") {
		if strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}

func servePage(w http.ResponseWriter, r *http.Request, files fs.FS) {
	body, err := fs.ReadFile(files, Index)
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, notBuilt, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, Index, time.Time{}, bytes.NewReader(body))
}

func serveFile(w http.ResponseWriter, r *http.Request, files fs.FS, name string) {
	f, err := files.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	rs, seekable := f.(io.ReadSeeker)
	if err != nil || !info.Mode().IsRegular() || !seekable {
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(name, licenseDir+"/") {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	http.ServeContent(w, r, name, time.Time{}, rs)
}
