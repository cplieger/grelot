//go:build !windows

package auth

import (
	"slices"

	"github.com/cplieger/access"
	"github.com/cplieger/grelot/internal/wire"
)

// Role is what a principal may act as. Every principal has exactly one.
type Role uint8

// The roles.
const (
	// RoleNone is refused by every scope: a recovery session, and any
	// principal no built-in verifier produced.
	RoleNone Role = iota
	// RoleAgent is a key acting as its own identities.
	RoleAgent
	// RoleOwner is the person: a full session, or a key holding the owner
	// scope.
	RoleOwner
)

// RoleOf is p's role. A key without the owner scope is an agent key, a
// key minted with no scope included; a key holding both scopes is an owner
// key, so no key acts as an identity on one row and the person on the next.
// Only access's IsSession and IsAPIKey decide the kind: a recovery session
// or a custom verifier's principal can carry a session ID or a key ID too.
func RoleOf(p access.Principal) Role { //nolint:gocritic // hugeParam: gate.Config.Allow passes a Principal value
	switch {
	case p.IsSession():
		return RoleOwner
	case !p.IsAPIKey():
		return RoleNone
	case p.HasScope(wire.ScopeOwner):
		return RoleOwner
	default:
		return RoleAgent
	}
}

// Allow is the gate's one permission check: whether scope, a row's rule,
// admits p's role. A scope it does not know admits no one.
func Allow(p access.Principal, scope string) bool { //nolint:gocritic // hugeParam: gate.Config.Allow's signature
	role := RoleOf(p)
	switch scope {
	case wire.ScopeOwner:
		return role == RoleOwner
	case wire.ScopeAgent:
		return role == RoleAgent
	case wire.ScopeAgentOrOwner:
		return role != RoleNone
	default:
		return false
	}
}

// keyScope is the scope a key's scopes amount to, as RoleOf reads them.
func keyScope(scopes []string) string {
	if slices.Contains(scopes, wire.ScopeOwner) {
		return wire.ScopeOwner
	}
	return wire.ScopeAgent
}
