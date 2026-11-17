package wirespec

import (
	"cmp"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

const (
	binaryPkg = "github.com/cplieger/grelot"
	wirePkg   = "github.com/cplieger/grelot/internal/wire"
)

// codegen is what wiregen links: it loads packages through
// golang.org/x/tools/go/packages, so only cmd/wire-codegen and tests may
// reach it, through this package.
var codegen = []string{
	"github.com/cplieger/grelot/internal/wirespec",
	"github.com/cplieger/go/wiregen/",
	"golang.org/x/tools/",
}

func TestBinaryImportsNoCodegen(t *testing.T) {
	for _, tags := range []string{"", "noui"} {
		t.Run("tags "+cmp.Or(tags, "none"), func(t *testing.T) {
			for _, p := range goList(t, "-deps", "-tags="+tags, binaryPkg) {
				for _, c := range codegen {
					if p == strings.TrimSuffix(c, "/") || strings.HasPrefix(p, c) {
						t.Errorf("the binary imports %s", p)
					}
				}
			}
		})
	}
	t.Run("the wire package imports the standard library only", func(t *testing.T) {
		for _, p := range goList(t, "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", wirePkg) {
			if p != wirePkg {
				t.Errorf("internal/wire imports %s", p)
			}
		}
	})
}

// goList runs go list with args and returns its non-empty lines.
func goList(t *testing.T, args ...string) []string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "go", append([]string{"list"}, args...)...).Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			t.Fatalf("go list %v: %v\n%s", args, err, ee.Stderr)
		}
		t.Fatalf("go list %v: %v", args, err)
	}
	return strings.Fields(string(out))
}
