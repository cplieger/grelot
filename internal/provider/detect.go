package provider

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// DetectError is detection finding no single host. Matched lists the
// providers tied on the most variables, empty when none matched; Error is the
// one line every identity command prints, with the fix.
type DetectError struct {
	Matched []string
}

func (e *DetectError) Error() string {
	matched := "none matched"
	if len(e.Matched) > 0 {
		matched = "matched " + strings.Join(e.Matched, ", ")
	}
	return fmt.Sprintf("grelot cannot tell which host this shell runs in (%s); run with --provider <name>, or set provider: <name> in the client config", matched)
}

// NoSessionError is a provider whose file records no session variable, so
// the shell has no session to bind and no command may send one.
type NoSessionError struct {
	Provider string
	Env      string
}

func (e *NoSessionError) Error() string {
	if e.Env == "" {
		return fmt.Sprintf("provider %s records no session variable, so grelot has no session to bind in this shell; run with --provider <name> for the host this shell runs in", e.Provider)
	}
	return fmt.Sprintf("provider %s keeps its session id in %s, which this shell does not set; run with --provider <name> for the host this shell runs in", e.Provider, e.Env)
}

// Detect picks the provider for this shell. override is the --provider flag
// or the client config's provider:, and wins when set. Otherwise the provider
// matching the most detect.env variables in lookup wins; a tie goes to the
// provider whose session.process is the nearest of ancestors (the client's
// ancestor process names, nearest first) only when every tied provider names
// one. A file that extends another has no detect (Load refuses it), so it is
// chosen only by name.
func (s *Set) Detect(override string, lookup func(name string) (string, bool), ancestors []string) (*Provider, error) {
	if override != "" {
		return s.Get(override)
	}
	best, tied := 0, []*Provider(nil)
	for _, name := range slices.Sorted(maps.Keys(s.valid)) {
		p := s.valid[name]
		n := 0
		for _, v := range p.Detect.Env {
			if _, ok := lookup(v); ok {
				n++
			}
		}
		switch {
		case n == 0 || n < best:
		case n > best:
			best, tied = n, append(tied[:0], p)
		default:
			tied = append(tied, p)
		}
	}
	if len(tied) == 1 {
		return tied[0], nil
	}
	if p := nearest(tied, ancestors); p != nil {
		return p, nil
	}
	err := &DetectError{}
	for _, p := range tied {
		err.Matched = append(err.Matched, p.Name)
	}
	return nil, err
}

// nearest breaks a tie by the nearest ancestor that exactly one tied provider
// names, or answers nil when the tie stands.
func nearest(tied []*Provider, ancestors []string) *Provider {
	if len(tied) == 0 || slices.ContainsFunc(tied, func(p *Provider) bool { return len(p.Session.Process) == 0 }) {
		return nil
	}
	for _, proc := range ancestors {
		var hit []*Provider
		for _, p := range tied {
			if slices.Contains(p.Session.Process, proc) {
				hit = append(hit, p)
			}
		}
		switch len(hit) {
		case 0:
			continue
		case 1:
			return hit[0]
		default:
			return nil
		}
	}
	return nil
}

// SessionID reads the host session id from session.env. It never answers an
// empty id: a file with no session.env, or a shell that leaves it unset or
// empty, answers a NoSessionError.
func (p *Provider) SessionID(lookup func(name string) (string, bool)) (string, error) {
	if p.Session.Env == "" {
		return "", &NoSessionError{Provider: p.Name}
	}
	if v, ok := lookup(p.Session.Env); ok && v != "" {
		return v, nil
	}
	return "", &NoSessionError{Provider: p.Name, Env: p.Session.Env}
}
