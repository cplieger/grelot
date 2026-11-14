//go:build !windows

package store

import (
	"fmt"
	"strings"
	"unicode"
)

// address wakes, for a path post, only the identities its opening
// mentions name, as wakeNamed says, and every other recipient gets the
// post quietly. When no mention names an identity the post can reach,
// rt.woken stays nil and the post wakes everyone, as any other post does.
// Each name of no identity, live or dormant, adds a notice, and one more
// says so when every name is such.
func (w *wtx) address(me *identityRow, rt *route, body string) error {
	if rt.thread.Kind != kindPath {
		return nil
	}
	names := mentions(body)
	unknown := 0
	for _, name := range names {
		found, err := w.wakeNamed(me, rt, name)
		if err != nil {
			return err
		}
		if !found {
			unknown++
			rt.notices = append(rt.notices, fmt.Sprintf("@%s is not registered; nobody of that name gets this", name))
		}
	}
	if unknown > 0 && unknown == len(names) {
		rt.notices = append(rt.notices, "no name it opens with is registered, so it woke every follower")
	}
	return nil
}

// wakeNamed has the post wake the identity named name, other than the
// sender, joining it to the recipients when a direct post would reach it;
// a dormant one adds directRoute's notice. It reports whether name names
// an identity, live or dormant, whether or not the post reaches it.
func (w *wtx) wakeNamed(me *identityRow, rt *route, name string) (bool, error) {
	peer, found, err := w.identityBy(w.s.t.byName, name)
	if err != nil || !found || peer.ID == me.ID {
		return found, err
	}
	if _, in := rt.recipients[peer.ID]; !in {
		if !reaches(me, &peer) || !reaches(&peer, me) {
			return true, nil
		}
		rt.recipients[peer.ID] = ""
	}
	if rt.woken == nil {
		rt.woken = map[uint64]bool{}
	}
	rt.woken[peer.ID] = true
	if peer.State == stateDormant {
		rt.notices = append(rt.notices, dormantNotice(&peer))
	}
	return true, nil
}

// reaches reports whether from's scope lets it post directly to to: an
// agent's always does, a delegate's only within its scope.
func reaches(from, to *identityRow) bool { return from.Parent == 0 || allowsAgent(from, to) }

// quiet reports whether recipient id gets the post without a wake.
func (rt *route) quiet(id uint64) bool { return rt.woken != nil && !rt.woken[id] }

// tally adds recipient r to out's split of an addressed post.
func (rt *route) tally(out *Posted, r *identityRow) {
	switch {
	case rt.woken == nil:
	case rt.quiet(r.ID):
		out.Quiet++
	default:
		out.Woken = append(out.Woken, r.Name)
	}
}

// mentions lists, once each and in order, the names of the @name or
// @parent/label tokens that open body. Spaces, commas and "and" separate
// them; the run ends at the first other token, or after a mention that
// ends in a colon.
func mentions(body string) []string {
	var out []string
	seen := map[string]bool{}
	sep := func(r rune) bool { return unicode.IsSpace(r) || r == ',' }
	rest := body
	for {
		rest = strings.TrimLeftFunc(rest, sep)
		end := strings.IndexFunc(rest, sep)
		if end < 0 {
			end = len(rest)
		}
		tok := rest[:end]
		rest = rest[end:]
		if tok == "and" && len(out) > 0 {
			continue
		}
		tok, last := strings.CutSuffix(tok, ":")
		name, ok := strings.CutPrefix(tok, "@")
		if !ok || (!namePattern.MatchString(name) && !isHandle(name)) {
			return out
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
		if last {
			return out
		}
	}
}
