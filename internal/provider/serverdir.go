package provider

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"

	"github.com/cplieger/go/envx"
)

// ServerDoor is one http door the server pushes through, with the file it
// came from.
type ServerDoor struct {
	Door *Door
	File string
}

// ServerDoors are the http doors of the server's providers folder, their
// secrets resolved.
type ServerDoors struct {
	doors   []ServerDoor
	secrets []string
}

// LoadServerDir loads the server's providers folder: every *.yaml file at its
// root decoded into the full schema, extends left unresolved, and only the
// http doors kept. A file that fails to load, whose secret cannot be read, or
// whose door can match an identity an earlier file's door (by file name) also
// matches is skipped and returned in skipped, for the caller's one
// "provider invalid" line each; no error text carries a value. A folder that
// does not exist holds no doors; another read error is returned.
func LoadServerDir(fsys fs.FS) (doors *ServerDoors, skipped []*LoadError, err error) {
	files, err := readLayers([]fs.FS{fsys})
	if err != nil {
		return nil, nil, err
	}
	doors = &ServerDoors{}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		p, err := Load(name, files[name])
		if err != nil {
			le, ok := errors.AsType[*LoadError](err)
			if !ok {
				le = &LoadError{Provider: name, Problem: err.Error()}
			}
			skipped = append(skipped, le)
			continue
		}
		if le := doors.add(p); le != nil {
			skipped = append(skipped, le)
		}
	}
	return doors, skipped, nil
}

// add keeps p's http doors when every secret resolves and no door conflicts,
// all or none of them.
func (s *ServerDoors) add(p *Provider) *LoadError {
	fail := func(door, problem string) *LoadError {
		return &LoadError{Provider: p.Name, Key: "doors", Problem: "door " + door + ": " + problem}
	}
	var kept []ServerDoor
	var values []string
	for i := range p.Doors {
		d := &p.Doors[i]
		if d.Transport != TransportHTTP {
			continue
		}
		for _, other := range slices.Concat(s.doors, kept) {
			if overlaps(d.Match, other.Door.Match) {
				return fail(d.Name, fmt.Sprintf("can serve the identities door %s of provider %s serves", other.Door.Name, other.File))
			}
		}
		resolved, problem := resolveSecrets(d)
		if problem != "" {
			return fail(d.Name, problem)
		}
		kept = append(kept, ServerDoor{Door: d, File: p.Name})
		values = append(values, resolved...)
	}
	s.doors = append(s.doors, kept...)
	s.secrets = append(s.secrets, values...)
	return nil
}

// resolveSecrets reads each secret d's headers name and binds it, returning
// the values, or the problem that names the failed one.
func resolveSecrets(d *Door) (values []string, problem string) {
	resolved := map[string]string{}
	for _, name := range d.SecretNames() {
		v, err := envx.Secret(secretKey(name))
		if err != nil {
			return nil, secretProblem(name, err)
		}
		resolved[name] = v
		values = append(values, v)
	}
	if err := d.ResolveSecrets(func(name string) (string, error) { return resolved[name], nil }); err != nil {
		return nil, err.Error()
	}
	return values, ""
}

// overlaps reports whether two match rules can select one identity: the same
// provider, and the same host or either one left out.
func overlaps(a, b Match) bool {
	return a.Provider == b.Provider && (a.Host == "" || b.Host == "" || a.Host == b.Host)
}

// secretKey is the variable a secret name reads. Load admits only
// [a-z0-9_]+ names, so the key is always a valid name and envx never panics.
func secretKey(name string) envx.Key { return envx.Key("SECRET_" + strings.ToUpper(name)) }

// secretProblem names why a secret failed by its envx class, never with the
// error's own text: a _FILE error carries the path, which a misconfigured
// deployment may have set to the secret itself.
func secretProblem(name string, err error) string {
	key := string(secretKey(name))
	file := key + "_FILE"
	_, missing := errors.AsType[*envx.MissingError](err)
	switch {
	case missing:
		return fmt.Sprintf("secret %s: %s is not set", name, key)
	case errors.Is(err, envx.ErrBlankSecretFile):
		return fmt.Sprintf("secret %s: %s names a blank file", name, file)
	case errors.Is(err, envx.ErrSecretFilePathRejected):
		return fmt.Sprintf("secret %s: %s must be a clean path with no .. component", name, file)
	case errors.Is(err, envx.ErrSecretFileTooLarge), errors.Is(err, envx.ErrSecretFileGrew):
		return fmt.Sprintf("secret %s: %s names a file over the size limit", name, file)
	default:
		return fmt.Sprintf("secret %s: %s names a file that cannot be read", name, file)
	}
}

// For returns the door that serves an identity of provider on host, if any.
func (s *ServerDoors) For(provider, host string) (ServerDoor, bool) {
	for _, d := range s.doors {
		if m := d.Door.Match; m.Provider == provider && (m.Host == "" || m.Host == host) {
			return d, true
		}
	}
	return ServerDoor{}, false
}

// All lists the loaded doors, sorted by file name, then as listed.
func (s *ServerDoors) All() []ServerDoor { return slices.Clone(s.doors) }

// Secrets lists every resolved secret value, the redaction needles for the
// server's log lines.
func (s *ServerDoors) Secrets() []string { return slices.Clone(s.secrets) }
