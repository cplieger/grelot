//go:build unix

package canon

import (
	"strings"
	"testing"
)

func TestScanLocal_otherCaseIsAnotherPath(t *testing.T) {
	h := newScanHost(t)
	if got, ok := h.r.ScanLocal(t.Context(), "see "+strings.ToUpper(h.temp)+"/a"); ok {
		t.Errorf("got %+v, want none: paths are case-sensitive here", got)
	}
}
