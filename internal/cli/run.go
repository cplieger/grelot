package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/cplieger/grelot/internal/canon"
	"github.com/cplieger/grelot/internal/client"
	"github.com/cplieger/grelot/internal/proctree"
	"github.com/cplieger/grelot/internal/provider"
	"github.com/cplieger/grelot/internal/where"
)

// run is one command's state. Each piece is read once, when first needed,
// so a command pays only for what it uses.
type run struct {
	*Env
	cfg          *client.Config
	prov         *provider.Provider
	api          *client.API
	resolver     *canon.Resolver
	place        *client.Where
	verb         string
	providerFlag string
	// handle is the delegate a grelot as command acts as.
	handle string
	// hookSession is the session a hook acts as, set when it runs.
	hookSession string
	root        string
	ancestors   []proctree.Process
	readTree    bool
	self        bool
	statusShown bool
}

func (r *run) config() (*client.Config, error) {
	if r.cfg != nil {
		return r.cfg, nil
	}
	cfg, err := client.LoadConfig(r.ConfigDir, r.Lookup, r.Hostname)
	if err != nil {
		return nil, err
	}
	r.cfg = cfg
	return cfg, nil
}

func (r *run) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *run) tree() []proctree.Process {
	if !r.readTree {
		r.readTree = true
		if r.Processes != nil {
			r.ancestors = proctree.Ancestors(r.Processes, r.PID)
		}
	}
	return r.ancestors
}

func (r *run) provider() (*provider.Provider, error) {
	if r.prov != nil {
		return r.prov, nil
	}
	cfg, err := r.config()
	if err != nil {
		return nil, err
	}
	p, err := client.Choose(r.Providers, r.providerFlag, cfg, client.Shell{Lookup: r.Lookup, Ancestors: proctree.Names(r.tree())})
	if err != nil {
		return nil, err
	}
	r.prov = p
	return p, nil
}

// client is the API for this command. An identity command needs the
// session; any other sends it when the shell has one.
func (r *run) client(identity bool) (*client.API, error) {
	if r.api != nil {
		return r.api, nil
	}
	cfg, err := r.config()
	if err != nil {
		return nil, err
	}
	newAPI := client.NewAPI
	if r.verb == verbHook {
		newAPI = client.NewHookAPI
	}
	a, err := newAPI(cfg, r.Version, r.ConfigDir)
	if err != nil {
		return nil, err
	}
	session, err := r.session()
	if err != nil && identity {
		return nil, err
	}
	a.Session, a.Self, a.As = session, r.self, r.handle
	r.api = a
	return a, nil
}

func (r *run) session() (string, error) {
	if r.hookSession != "" {
		return r.hookSession, nil
	}
	p, err := r.provider()
	if err != nil {
		return "", err
	}
	return client.Session(p, r.Lookup)
}

// call sends req, an identity request, as this command's answer and
// prints its status line.
func (r *run) call(ctx context.Context, req *client.Request, out any) error {
	a, err := r.client(true)
	if err != nil {
		return err
	}
	resp, err := a.Do(ctx, r.bounded(req), out)
	if err != nil {
		return err
	}
	r.printStatus(resp.Status)
	return nil
}

func (r *run) bounded(req *client.Request) *client.Request {
	if req.Timeout == 0 {
		req.Timeout = r.Timeout
	}
	return req
}

// peek sends req before the command's answer, printing nothing.
func (r *run) peek(ctx context.Context, req *client.Request, out any) error {
	a, err := r.client(true)
	if err != nil {
		return err
	}
	_, err = a.Do(ctx, r.bounded(req), out)
	return err
}

// peekMe reads GET /v1/me before the command's answer.
func (r *run) peekMe(ctx context.Context) (*client.Me, error) {
	a, err := r.client(true)
	if err != nil {
		return nil, err
	}
	me, _, err := a.Me(ctx, url.Values{}, http.Header{})
	return me, err
}

// statusLine is the line printed before an answer whose Grelot-Status is
// server.
func (r *run) statusLine(server string) string {
	s := client.Status{
		Server: server, Client: r.Version,
		ServerVersion: client.LoadFacts(r.ConfigDir).Version, Invalid: client.Invalid(r.Providers),
	}
	if r.prov != nil {
		s.Start = client.FirstStart(r.prov)
	}
	return s.Line()
}

func (r *run) printStatus(server string) {
	if r.statusShown {
		return
	}
	r.statusShown = true
	if line := r.statusLine(server); line != "" {
		fmt.Fprintln(r.Stdout, line)
	}
}

func (r *run) paths() (*canon.Resolver, error) {
	if r.resolver != nil {
		return r.resolver, nil
	}
	cfg, err := r.config()
	if err != nil {
		return nil, err
	}
	r.root = client.LoadFacts(r.ConfigDir).PathRoot
	r.resolver = canon.New(r.root, cfg.Repos, canon.NewClones(r.ConfigDir))
	return r.resolver, nil
}

// machineAndEnvironment are this process's machine and environment ids,
// read once.
func (r *run) machineAndEnvironment(ctx context.Context) client.Where {
	if r.place == nil {
		w := client.Where{Environment: where.Environment(ctx, where.EnvironmentFile(r.ConfigDir), r.InContainer())}
		w.Machine = r.Machine(ctx)
		r.place = &w
	}
	return *r.place
}
