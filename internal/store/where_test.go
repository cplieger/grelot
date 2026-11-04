package store

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// hex32 is a 32-digit hex value of one repeated digit, built at run time so
// no key-shaped literal sits in the source.
func hex32(digit string) string { return strings.Repeat(digit, 32) }

func dmi(digit string) string       { return "dmi:" + hex32(digit) }
func machineID(digit string) string { return "machine-id:" + hex32(digit) }

const (
	repoSubflux = "/workspace/subflux"
	repoGrelot  = "/workspace/grelot"
	repoInner   = "/workspace/subflux/vendor/inner"
)

func co(repo, digit string) Checkout { return Checkout{Repo: repo, ID: hex32(digit)} }

func TestRelation_ranks(t *testing.T) {
	tests := []struct {
		name           string
		reader, sender Where
		path           string
		want           string
	}{
		{
			name:   "one checkout in one environment",
			reader: Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "c")}},
			sender: Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "c")}},
			path:   repoSubflux + "/internal/x.go",
			want:   "same checkout",
		},
		{
			name:   "one environment, a path outside every repo",
			reader: Where{Machine: dmi("1"), Environment: hex32("a")},
			sender: Where{Machine: dmi("1"), Environment: hex32("a")},
			path:   "/workspace/_scratch/notes.md",
			want:   "same environment",
		},
		{
			name:   "two worktrees of one clone",
			reader: Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "c")}},
			sender: Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "d")}},
			path:   repoSubflux + "/go.mod",
			want:   "same environment, other checkout of " + repoSubflux,
		},
		{
			name:   "a cp -a copy of the clone, its id recomputed at the new path",
			reader: Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "c")}},
			sender: Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "e")}},
			path:   repoSubflux,
			want:   "same environment, other checkout of " + repoSubflux,
		},
		{
			name:   "one side with no entry for the path's repo",
			reader: Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "c")}},
			sender: Where{Machine: dmi("1"), Environment: hex32("a")},
			path:   repoSubflux + "/go.mod",
			want:   "same environment, checkout of " + repoSubflux + " unknown",
		},
		{
			name:   "equal checkout ids read without a path",
			reader: Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "c")}},
			sender: Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "c")}},
			want:   "same environment",
		},
		{
			name:   "one machine, two known environments",
			reader: Where{Machine: dmi("1"), Environment: hex32("a")},
			sender: Where{Machine: dmi("1"), Environment: hex32("b")},
			want:   "same machine, other environment",
		},
		{
			name:   "one machine, one environment empty",
			reader: Where{Machine: dmi("1"), Environment: hex32("a")},
			sender: Where{Machine: dmi("1")},
			want:   "same machine, environment unknown",
		},
		{
			name:   "one machine, both environments empty",
			reader: Where{Machine: dmi("1")},
			sender: Where{Machine: dmi("1")},
			want:   "same machine, environment unknown",
		},
		{
			name:   "two machines of one kind",
			reader: Where{Machine: dmi("1"), Environment: hex32("a")},
			sender: Where{Machine: dmi("2"), Environment: hex32("b")},
			want:   "other machine",
		},
		{
			name:   "an environment id copied to two machines",
			reader: Where{Machine: dmi("1"), Environment: hex32("a")},
			sender: Where{Machine: dmi("2"), Environment: hex32("a")},
			want:   "other machine",
		},
		{
			name:   "a cp -a copy with its environment file on another machine of the same kind",
			reader: Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "c")}},
			sender: Where{Machine: dmi("2"), Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "c")}},
			path:   repoSubflux + "/go.mod",
			want:   "other machine",
		},
		{
			name:   "two machine kinds",
			reader: Where{Machine: dmi("1"), Environment: hex32("a")},
			sender: Where{Machine: machineID("2"), Environment: hex32("b")},
			want:   "unknown",
		},
		{
			name:   "an environment id copied to two machines of two kinds",
			reader: Where{Machine: dmi("1"), Environment: hex32("a")},
			sender: Where{Machine: machineID("1"), Environment: hex32("a")},
			want:   "unknown",
		},
		{
			name:   "both environments empty, no machine",
			reader: Where{},
			sender: Where{},
			want:   "unknown",
		},
		{
			name:   "equal checkout ids, two environments",
			reader: Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "c")}},
			sender: Where{Machine: dmi("1"), Environment: hex32("b"), Checkouts: []Checkout{co(repoSubflux, "c")}},
			path:   repoSubflux + "/go.mod",
			want:   "same machine, other environment",
		},
		{
			name:   "equal checkout ids, one environment empty",
			reader: Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "c")}},
			sender: Where{Machine: dmi("1"), Checkouts: []Checkout{co(repoSubflux, "c")}},
			path:   repoSubflux + "/go.mod",
			want:   "same machine, environment unknown",
		},
		{
			name:   "a copied environment, one machine unknown, equal checkout ids",
			reader: Where{Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "c")}},
			sender: Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "c")}},
			path:   repoSubflux + "/go.mod",
			want:   "same checkout",
		},
		{
			name:   "a container agent beside its host's agent on a shared home",
			reader: Where{Environment: hex32("a")},
			sender: Where{Machine: machineID("1"), Environment: hex32("b")},
			want:   "unknown",
		},
		{
			name:   "two containers on one home volume",
			reader: Where{Environment: hex32("a")},
			sender: Where{Environment: hex32("a")},
			want:   "same environment",
		},
		{
			name:   "a cloned VM sending one machine and one environment",
			reader: Where{Machine: machineID("1"), Environment: hex32("a")},
			sender: Where{Machine: machineID("1"), Environment: hex32("a")},
			want:   "same environment",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := relation(tt.reader, tt.sender, tt.path); got != tt.want {
				t.Errorf("relation(reader, sender) = %q, want %q", got, tt.want)
			}
			if got := relation(tt.sender, tt.reader, tt.path); got != tt.want {
				t.Errorf("relation(sender, reader) = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRelation_perRepo(t *testing.T) {
	reader := Where{
		Machine: dmi("1"), Environment: hex32("a"),
		Checkouts: []Checkout{co(repoSubflux, "c"), co(repoGrelot, "d")},
	}
	sender := Where{
		Machine: dmi("1"), Environment: hex32("a"),
		Checkouts: []Checkout{co(repoSubflux, "c"), co(repoGrelot, "e")},
	}
	if got, want := relation(reader, sender, repoSubflux+"/x.go"), "same checkout"; got != want {
		t.Errorf("post in the shared repo: %q, want %q", got, want)
	}
	if got, want := relation(reader, sender, repoGrelot+"/x.go"), "same environment, other checkout of "+repoGrelot; got != want {
		t.Errorf("post in the other repo: %q, want %q", got, want)
	}
	if got, want := relation(reader, sender, "/workspace/subflux-old/x.go"), "same environment"; got != want {
		t.Errorf("a sibling directory sharing a name prefix: %q, want %q", got, want)
	}
}

func TestRelation_nestedRepo(t *testing.T) {
	both := Where{
		Machine: dmi("1"), Environment: hex32("a"),
		Checkouts: []Checkout{co(repoSubflux, "c"), co(repoInner, "f")},
	}
	outerOnly := Where{
		Machine: dmi("1"), Environment: hex32("a"),
		Checkouts: []Checkout{co(repoSubflux, "c")},
	}
	inner := repoInner + "/lib.go"
	if got, want := relation(both, both, inner), "same checkout"; got != want {
		t.Errorf("both sides hold the inner repo: %q, want %q", got, want)
	}
	if got, want := relation(both, outerOnly, inner), "same environment, checkout of "+repoInner+" unknown"; got != want {
		t.Errorf("one side lacks the inner repo: %q, want %q", got, want)
	}
	if got, want := relation(outerOnly, both, inner), "same environment, checkout of "+repoInner+" unknown"; got != want {
		t.Errorf("one side lacks the inner repo, reversed: %q, want %q", got, want)
	}
	if got, want := relation(both, outerOnly, repoSubflux+"/x.go"), "same checkout"; got != want {
		t.Errorf("a path in the outer repo only: %q, want %q", got, want)
	}
}

func TestRelationTo_youAndServerEvents(t *testing.T) {
	w := Where{Machine: dmi("1"), Environment: hex32("a")}
	reader := party{id: 7, where: w}
	if got, want := reader.relationTo(party{id: 7, where: Where{}}, ""), "you"; got != want {
		t.Errorf("the reader's own post: %q, want %q", got, want)
	}
	if got := reader.relationTo(party{where: w}, ""); got != "" {
		t.Errorf("a server event: %q, want no relation", got)
	}
	if got, want := reader.relationTo(party{id: 8, where: w}, ""), "same environment"; got != want {
		t.Errorf("another identity: %q, want %q", got, want)
	}
}

func TestShareEnvironment(t *testing.T) {
	tests := []struct {
		name string
		a, b Where
		want bool
	}{
		{"one environment, one machine", Where{Machine: dmi("1"), Environment: hex32("a")}, Where{Machine: dmi("1"), Environment: hex32("a")}, true},
		{"one environment, one machine unknown", Where{Environment: hex32("a")}, Where{Machine: dmi("1"), Environment: hex32("a")}, true},
		{"one environment, both machines unknown", Where{Environment: hex32("a")}, Where{Environment: hex32("a")}, true},
		{"copied to another machine of one kind", Where{Machine: dmi("1"), Environment: hex32("a")}, Where{Machine: dmi("2"), Environment: hex32("a")}, false},
		{"copied to a machine of another kind", Where{Machine: dmi("1"), Environment: hex32("a")}, Where{Machine: machineID("1"), Environment: hex32("a")}, false},
		{"both environments empty", Where{Machine: dmi("1")}, Where{Machine: dmi("1")}, false},
		{"one environment empty", Where{Machine: dmi("1"), Environment: hex32("a")}, Where{Machine: dmi("1")}, false},
		{"two environments", Where{Machine: dmi("1"), Environment: hex32("a")}, Where{Machine: dmi("1"), Environment: hex32("b")}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shareEnvironment(tt.a, tt.b); got != tt.want {
				t.Errorf("shareEnvironment(a, b) = %v, want %v", got, tt.want)
			}
			if got := shareEnvironment(tt.b, tt.a); got != tt.want {
				t.Errorf("shareEnvironment(b, a) = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCheckWhere_accepts(t *testing.T) {
	paths := []string{repoSubflux + "/internal", repoGrelot}
	tests := []struct {
		name  string
		where Where
	}{
		{"every member empty", Where{}},
		{"every machine kind", Where{Machine: "macos:" + hex32("0")}},
		{"a windows machine", Where{Machine: "windows:" + hex32("f")}},
		{"a machine-id machine", Where{Machine: machineID("9")}},
		{"a dmi machine and an environment", Where{Machine: dmi("1"), Environment: hex32("a")}},
		{"a checkout per repo, one at the work path itself", Where{Checkouts: []Checkout{co(repoSubflux, "c"), co(repoGrelot, "d")}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := checkWhere(&tt.where, paths); err != nil {
				t.Errorf("checkWhere: %v, want accepted", err)
			}
		})
	}
}

func TestCheckWhere_refuses(t *testing.T) {
	paths := []string{repoSubflux + "/internal", repoGrelot}
	many := make([]Checkout, 17)
	for i := range many {
		many[i] = co(repoSubflux+"/r"+strconv.Itoa(i), "c")
	}
	tests := []struct {
		name  string
		where *Where
		field string
	}{
		{"missing", nil, "where"},
		{"a machine with no kind", &Where{Machine: hex32("1")}, "where.machine"},
		{"a machine of an unknown kind", &Where{Machine: "bios:" + hex32("1")}, "where.machine"},
		{"an upper-case kind", &Where{Machine: "DMI:" + hex32("1")}, "where.machine"},
		{"an upper-case machine hash", &Where{Machine: "dmi:" + hex32("A")}, "where.machine"},
		{"a short machine hash", &Where{Machine: "dmi:" + hex32("1")[:31]}, "where.machine"},
		{"a long machine hash", &Where{Machine: dmi("1") + "1"}, "where.machine"},
		{"a short environment", &Where{Environment: hex32("a")[:31]}, "where.environment"},
		{"an upper-case environment", &Where{Environment: hex32("A")}, "where.environment"},
		{"a non-hex environment", &Where{Environment: hex32("g")}, "where.environment"},
		{"seventeen checkouts", &Where{Checkouts: many}, "where.checkouts"},
		{"a relative repo", &Where{Checkouts: []Checkout{co("workspace/subflux", "c")}}, "where.checkouts[0].repo"},
		{"an unclean repo", &Where{Checkouts: []Checkout{co(repoSubflux+"/", "c")}}, "where.checkouts[0].repo"},
		{"a repo with ..", &Where{Checkouts: []Checkout{co("/workspace/x/../subflux", "c")}}, "where.checkouts[0].repo"},
		{"a repo no work path lies in", &Where{Checkouts: []Checkout{co(repoGrelot, "d"), co("/workspace/animap", "c")}}, "where.checkouts[1].repo"},
		{"a repo below the work path", &Where{Checkouts: []Checkout{co(repoSubflux+"/internal/x", "c")}}, "where.checkouts[0].repo"},
		{"a repo twice", &Where{Checkouts: []Checkout{co(repoGrelot, "d"), co(repoGrelot, "e")}}, "where.checkouts[1].repo"},
		{"a short checkout id", &Where{Checkouts: []Checkout{{Repo: repoGrelot, ID: hex32("d")[:31]}}}, "where.checkouts[0].id"},
		{"an upper-case checkout id", &Where{Checkouts: []Checkout{co(repoGrelot, "D")}}, "where.checkouts[0].id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkWhere(tt.where, paths)
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("checkWhere: %v, want a refusal", err)
			}
			if e.Kind != KindInvalid {
				t.Errorf("kind %v, want KindInvalid", e.Kind)
			}
			if !strings.HasPrefix(e.Msg, tt.field+" ") {
				t.Errorf("message %q does not lead with the field %s", e.Msg, tt.field)
			}
		})
	}
}

func TestMergeCheckouts(t *testing.T) {
	paths := []string{repoSubflux, repoGrelot + "/cmd"}
	stored := []Checkout{co(repoSubflux, "c"), co(repoGrelot, "d")}
	t.Run("a renew from one clone replaces its repo and keeps the others", func(t *testing.T) {
		got := byRepo(mergeCheckouts(stored, []Checkout{co(repoGrelot, "e")}, paths))
		want := []Checkout{co(repoGrelot, "e"), co(repoSubflux, "c")}
		if !slices.Equal(got, want) {
			t.Errorf("merged %v, want %v", got, want)
		}
	})
	t.Run("a renew from outside every clone keeps every stored entry", func(t *testing.T) {
		got := byRepo(mergeCheckouts(stored, nil, paths))
		want := []Checkout{co(repoGrelot, "d"), co(repoSubflux, "c")}
		if !slices.Equal(got, want) {
			t.Errorf("merged %v, want %v", got, want)
		}
	})
	t.Run("a repo no work path lies in any more loses its entry", func(t *testing.T) {
		got := byRepo(mergeCheckouts(stored, nil, []string{repoSubflux}))
		want := []Checkout{co(repoSubflux, "c")}
		if !slices.Equal(got, want) {
			t.Errorf("merged %v, want %v", got, want)
		}
	})
	t.Run("the stored slice is not modified", func(t *testing.T) {
		before := slices.Clone(stored)
		_ = mergeCheckouts(stored, []Checkout{co(repoGrelot, "e")}, paths)
		if !slices.Equal(stored, before) {
			t.Errorf("stored became %v, want %v", stored, before)
		}
	})
}

func byRepo(cs []Checkout) []Checkout {
	slices.SortFunc(cs, func(a, b Checkout) int { return strings.Compare(a.Repo, b.Repo) })
	return cs
}
