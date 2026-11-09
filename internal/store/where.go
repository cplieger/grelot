//go:build !windows

package store

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/cplieger/bolttable"
	"github.com/cplieger/grelot/internal/protopath"
)

// Where is the client's report of where an identity runs: keyed hashes of
// its machine, its environment and one working tree per repo. The values
// gate the take-over and the revive, so no response or log line may carry
// them; only the relation computed from them is shown.
type Where struct {
	Machine     string     `json:"machine"`
	Environment string     `json:"environment"`
	Checkouts   []Checkout `json:"checkouts"`
}

// Checkout names one working tree of the repo whose canonical root is Repo.
// Remote is a keyed hash of the clone's normalized remote, so two clones
// that share a root's name but not a repository can be told apart.
type Checkout struct {
	Repo   string `json:"repo"`
	ID     string `json:"id"`
	Remote string `json:"remote"`
}

// maxCheckouts matches the 16 work paths an identity may hold.
const maxCheckouts = 16

var (
	machinePattern = regexp.MustCompile(`^(dmi|macos|windows|machine-id):[0-9a-f]{32}$`)
	hexPattern     = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// checkWhere validates w, which a register, revive or renew must send,
// against the identity's canonical work paths.
func checkWhere(w *Where, paths []string) error {
	if w == nil {
		return refuse(KindInvalid,
			"where is required: send {machine, environment, checkouts} with a register, revive or renew, and with a set or post that changes paths")
	}
	if w.Machine != "" && !machinePattern.MatchString(w.Machine) {
		return refuse(KindInvalid,
			`where.machine must be "" or <kind>:<32 lowercase hex digits>, kind one of dmi, macos, windows, machine-id`)
	}
	if w.Environment != "" && !hexPattern.MatchString(w.Environment) {
		return refuse(KindInvalid, `where.environment must be "" or 32 lowercase hex digits`)
	}
	if len(w.Checkouts) > maxCheckouts {
		return refuse(KindInvalid, fmt.Sprintf("where.checkouts holds at most %d entries", maxCheckouts))
	}
	seen := make(map[string]bool, len(w.Checkouts))
	for i, c := range w.Checkouts {
		field := fmt.Sprintf("where.checkouts[%d]", i)
		switch {
		case !isCanonicalPath(c.Repo):
			return refuse(KindInvalid, field+".repo must be a clean absolute path with no ..")
		case !slices.ContainsFunc(paths, func(p string) bool { return under(p, c.Repo) }):
			return refuse(KindInvalid, field+".repo must be a repo root one of your work paths lies in")
		case seen[c.Repo]:
			return refuse(KindInvalid, field+".repo repeats a repo: send one entry per repo")
		case !hexPattern.MatchString(c.ID):
			return refuse(KindInvalid, field+".id must be 32 lowercase hex digits")
		case !hexPattern.MatchString(c.Remote):
			return refuse(KindInvalid, field+".remote must be 32 lowercase hex digits")
		}
		seen[c.Repo] = true
	}
	return nil
}

// repoConflict refuses row's where when one of its checkouts names a repo
// root that another live identity's checkout gives another remote: two
// repositories with one name would otherwise share every path thread and
// reservation under that root.
func (w *wtx) repoConflict(row *identityRow) error {
	if len(row.Where.Checkouts) == 0 {
		return nil
	}
	var refusal error
	err := w.s.t.identities.table.Scan(w.r, bolttable.Range{}, func(_ uint64, v *identityRow) (bool, error) {
		if v = settled(v, w.now, w.s.dormantTTL); v == nil || v.ID == row.ID || v.State != stateLive {
			return true, nil
		}
		for _, mine := range row.Where.Checkouts {
			for _, theirs := range v.Where.Checkouts {
				if mine.Repo == theirs.Repo && mine.Remote != theirs.Remote {
					refusal = &Error{Kind: KindConflict, Why: "other_repository", Msg: fmt.Sprintf(
						"%s is another repository for %s; give one of them another name with `repos:` in your client config",
						mine.Repo, v.Name,
					)}
					return false, nil
				}
			}
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	return refusal
}

func isCanonicalPath(p string) bool { return protopath.IsCanonical(p) }

func under(p, root string) bool { return protopath.Under(p, root) }

// mergeCheckouts is the checkout rule of a revive or renew, which sends an
// entry only for the clone holding the working directory: sent replaces
// its repos' stored entries, the rest are kept, and a repo no work path
// lies in any more loses its entry.
func mergeCheckouts(stored, sent []Checkout, paths []string) []Checkout {
	out := slices.Clone(sent)
	for _, c := range stored {
		if slices.ContainsFunc(sent, func(s Checkout) bool { return s.Repo == c.Repo }) {
			continue
		}
		if slices.ContainsFunc(paths, func(p string) bool { return under(p, c.Repo) }) {
			out = append(out, c)
		}
	}
	return out
}

// shareEnvironment is the one predicate every environment rule uses, so
// the relation shown and the moves allowed never disagree: equal non-empty
// environments on machines not known to differ. Machine values compare as
// whole strings, so two kinds count as different machines.
func shareEnvironment(a, b Where) bool {
	return a.Environment != "" && a.Environment == b.Environment &&
		(a.Machine == "" || b.Machine == "" || a.Machine == b.Machine)
}

// relationUnknown is the relation when nothing is known to be shared, or
// the other side's row is gone.
const relationUnknown = "unknown"

// relation names what two identities share, best rank first. path is the
// one the read item names, or "" for a direct message, an announcement,
// who and the name refusals, which then read same environment at best.
// The pair is unordered: swapping a and b gives the same answer.
func relation(a, b Where, path string) string {
	if shareEnvironment(a, b) {
		return environmentRelation(a, b, path)
	}
	switch {
	case a.Machine != "" && a.Machine == b.Machine:
		if a.Environment != "" && b.Environment != "" {
			return "same machine, other environment"
		}
		return "same machine, environment unknown"
	case a.Machine != "" && b.Machine != "" && machineKind(a.Machine) == machineKind(b.Machine):
		return "other machine"
	case a.Machine != "" && b.Machine != "":
		kinds := []string{machineKind(a.Machine), machineKind(b.Machine)}
		slices.Sort(kinds)
		return "other environment, machine not comparable (" + kinds[0] + " · " + kinds[1] + ")"
	default:
		return relationUnknown
	}
}

// environmentRelation splits rank 2 by the checkout of the path's repo,
// the longest root among both sides' entries, so a nested repo is judged
// the same way from either side.
func environmentRelation(a, b Where, path string) string {
	repo := ""
	if path != "" {
		for _, c := range slices.Concat(a.Checkouts, b.Checkouts) {
			if len(c.Repo) > len(repo) && under(path, c.Repo) {
				repo = c.Repo
			}
		}
	}
	if repo == "" {
		return "same environment"
	}
	ida, oka := checkoutID(a, repo)
	idb, okb := checkoutID(b, repo)
	switch {
	case !oka || !okb:
		return "same environment, checkout of " + repo + " unknown"
	case ida == idb:
		return "same checkout"
	default:
		return "same environment, other checkout of " + repo
	}
}

func checkoutID(w Where, repo string) (string, bool) {
	i := slices.IndexFunc(w.Checkouts, func(c Checkout) bool { return c.Repo == repo })
	if i < 0 {
		return "", false
	}
	return w.Checkouts[i].ID, true
}

func machineKind(m string) string {
	kind, _, _ := strings.Cut(m, ":")
	return kind
}

// party is one side of a relation: an identity id, 0 for a server event,
// and the where it is judged by.
type party struct {
	where Where
	id    uint64
}

// relationTo is the relation a reader sees beside sender: you for its own
// post, none for a server event.
func (p party) relationTo(sender party, path string) string {
	switch sender.id {
	case 0:
		return ""
	case p.id:
		return "you"
	default:
		return relation(p.where, sender.where, path)
	}
}
