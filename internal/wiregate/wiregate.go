// Package wiregate is how the wire rows meet access: the gate class each
// row's Access maps to, the gate's codes a row can answer, and access's own
// routes under the wire prefixes. The server's gate and internal/wirespec
// both call it, so the gate that runs and the contract that is generated
// cannot disagree. It builds on every OS and holds no state.
package wiregate

import (
	"github.com/cplieger/access"
	"github.com/cplieger/access/accesshttp"
	"github.com/cplieger/access/gate"
	"github.com/cplieger/grelot/internal/wire"
)

// Route is r's gate row, named by its ServeMux pattern. It panics on an
// Access it does not map, which is a wiring bug in wire.Routes.
func Route(r *wire.Route) gate.Route {
	g := gate.Route{Name: r.Pattern(), Method: r.Method, Path: r.Path}
	switch r.Access {
	case wire.AccessAgent:
		g.Class = gate.Key
	case wire.AccessOwner:
		g.Class = gate.Session
	case wire.AccessPublic:
		g.Class = gate.Public
	default:
		panic("wiregate: row " + r.Name + " admits no one")
	}
	return g
}

// Codes is every code the gate answers on r: its row's, then the mount's
// for a path no row matches.
func Codes(r *wire.Route) []access.Code {
	return append(gate.Codes(Route(r)), gate.MountCodes()...)
}

// AuthDescriptors are access's routes under wire.AuthPrefix and
// wire.OIDCPrefix, with no key scope set, as internal/auth mounts them.
func AuthDescriptors() []accesshttp.Descriptor {
	return accesshttp.Descriptors(accesshttp.DescriptorConfig{Prefix: wire.AuthPrefix, OIDCPrefix: wire.OIDCPrefix})
}
