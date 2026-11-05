package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/cplieger/grelot/internal/canon"
)

func (r *run) path(ctx context.Context, args []string) error {
	fs := flags(r.verb)
	local := fs.Bool("local", false, "")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef(r.verb, "name one path: grelot path <local path>, or grelot path --local <grelot path>")
	}
	if !*local {
		canonical, _, cerr := r.canonical(ctx, rest)
		if cerr != nil {
			return cerr
		}
		fmt.Fprintln(r.Stdout, canonical[0])
		return nil
	}
	res, err := r.paths()
	if err != nil {
		return err
	}
	p, err := res.Local(ctx, rest[0])
	switch {
	case errors.Is(err, canon.ErrNotRepoPath):
		return fmt.Errorf("%s is not under %s, so it is local to the agent that sent it: ask it for the content with grelot post @<its name>", safe(rest[0]), r.root)
	case errors.Is(err, canon.ErrNoClone):
		return fmt.Errorf("no clone of the repository %s names is known on this machine: clone it, then run grelot path on a file inside it", safe(rest[0]))
	case err != nil:
		return err
	}
	fmt.Fprintln(r.Stdout, p)
	return nil
}
