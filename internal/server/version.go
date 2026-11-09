//go:build !windows

package server

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/version"
)

// minClient is the release base of the oldest client this server speaks.
// Raise it in the release whose API an older client can no longer use.
const minClient = "v1.1.0"

// speaks reports whether this server speaks a client of version v.
func speaks(v string) bool {
	if v == version.Dev {
		return true
	}
	got, ok := version.Parse(v)
	if !ok {
		return false
	}
	floor, _ := version.Parse(minClient)
	return slices.Compare(got.Release[:], floor.Release[:]) >= 0
}

// upgradeLine is the 426's one line: the client to install for a server of
// version server. It names the server's own tag, which exists for a dev
// build whose release base is not out yet.
func upgradeLine(client, server string) string {
	if client == "" {
		client = "with no Grelot-Version"
	}
	if _, ok := version.Parse(server); !ok {
		return fmt.Sprintf("this grelot server (%s) does not speak client %s; install the client built from the same source as the server", server, client)
	}
	return fmt.Sprintf("this grelot server (%s) does not speak client %s; install grelot %s with go install github.com/cplieger/grelot@%s or from https://github.com/cplieger/grelot/releases/tag/%s, then check both with grelot version",
		server, client, server, server, server)
}

// versionResponse is GET /v1/version.
type versionResponse struct {
	Version string `json:"version"`
}

// serverVersion is GET /v1/version, which any caller may make; the answer's
// Grelot-Version header names the same version.
func (s *Server) serverVersion(_ *http.Request, _ store.Caller) (status int, body any, err error) {
	return http.StatusOK, versionResponse{Version: s.version}, nil
}
