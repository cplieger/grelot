//go:build !windows

package server

import (
	"net/http"

	"github.com/cplieger/access"
	"github.com/cplieger/access/gate"
	"github.com/cplieger/grelot/internal/obs"
)

// table is the gate's endpoint table, filled as the routes register.
type table struct {
	handlers map[string]http.Handler
	rows     []gate.Route
}

func newTable() *table { return &table{handlers: map[string]http.Handler{}} }

// add serves row with h.
func (t *table) add(row gate.Route, h http.Handler) { //nolint:gocritic // hugeParam: a gate row, added once at startup
	t.rows = append(t.rows, row)
	t.handlers[row.Name] = h
}

// mount serves t through one gate on mux, on each row's path with no
// method, and on each catch-all prefix. A method-less pattern keeps the
// path as the access line's route label and leaves a wrong method to the
// gate, which answers it 405 with Allow.
func (s *Server) mount(mux *http.ServeMux, t *table, prefixes ...string) {
	g, err := gate.New(gate.Config{
		Authenticator: s.auth.Authenticator(), Errors: AuthErrors(s.log, s.metrics), Audit: s.audit, Logger: s.log,
	}, t.rows, t.handlers)
	if err != nil {
		panic("server: " + err.Error())
	}
	seen := map[string]bool{}
	for i := range t.rows {
		if p := t.rows[i].Path; !seen[p] {
			seen[p] = true
			mux.Handle(p, g)
		}
	}
	for _, p := range prefixes {
		mux.Handle(p, g)
	}
}

// audit counts each credential the gate refused, by its reason.
func (s *Server) audit(_ *http.Request, e access.AuditEvent) { //nolint:gocritic // hugeParam: gate.Config.Audit's signature
	if e.Kind == "gate.failure" || e.Kind == "gate.forbidden" {
		s.metrics.AuthFailures.Inc(obs.AuthReason(e.Reason))
	}
}
