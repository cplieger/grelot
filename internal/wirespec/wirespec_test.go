package wirespec

import (
	"testing"

	"github.com/cplieger/grelot/internal/wire"
)

// A route added to wire.Routes or to specs alone fails here, before the
// generator writes a document that lacks it or the server binds it bare.
func TestSpecCoversEveryRow(t *testing.T) {
	rows := map[string]bool{}
	for i := range wire.Routes {
		name := wire.Routes[i].Name
		if rows[name] {
			t.Errorf("two rows are named %s", name)
		}
		rows[name] = true
		if _, ok := specs[name]; !ok {
			t.Errorf("row %s has no spec", name)
		}
	}
	for name := range specs {
		if !rows[name] {
			t.Errorf("spec %s names no row", name)
		}
	}
}
