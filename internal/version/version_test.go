package version

import (
	"runtime/debug"
	"testing"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want V
		ok   bool
	}{
		{"v1.2.3", V{Release: [3]int{1, 2, 3}, Dev: -1}, true},
		{"v1.2.0-dev.31", V{Release: [3]int{1, 2, 0}, Dev: 31}, true},
		{"v0.0.0-dev.0", V{Dev: 0}, true},
		{"v999999999.0.0", V{Release: [3]int{999999999, 0, 0}, Dev: -1}, true},
		{"dev", V{}, false},
		{"", V{}, false},
		{"1.2.3", V{}, false},
		{"v1.2", V{}, false},
		{"v1.2.3.4", V{}, false},
		{"v01.2.3", V{}, false},
		{"v1.2.3-rc.1", V{}, false},
		{"v1.2.3-dev.", V{}, false},
		{"v1.2.3-dev.01", V{}, false},
		{"v1000000000.0.0", V{}, false},
		{"v1.2.0-dev.31.0.20261105101010-0123456789ab", V{}, false},
		{"v1.2.0-dev.31+dirty", V{}, false},
		{"v1.2.3+dirty", V{}, false},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := Parse(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Errorf("Parse(%q) = %+v, %v; want %+v, %v", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestBuild(t *testing.T) {
	module := func(v string) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{Main: debug.Module{Path: "example.com/m", Version: v}}, true
		}
	}
	none := func() (*debug.BuildInfo, bool) { return nil, false }
	for _, tc := range []struct {
		name  string
		stamp string
		read  func() (*debug.BuildInfo, bool)
		want  string
	}{
		{"a stamp wins", "v1.2.0", module("v1.1.0"), "v1.2.0"},
		{"a dev stamp reads the module", Dev, module("v1.2.0-dev.31"), "v1.2.0-dev.31"},
		{"no stamp reads the module", "", module("v1.2.0"), "v1.2.0"},
		{"a pseudo-version as go version -m shows it", "", module("v1.2.0-dev.31.0.20261105101010-0123456789ab"), "v1.2.0-dev.31.0.20261105101010-0123456789ab"},
		{"a dirty checkout", "", module("v1.2.0-dev.31+dirty"), "v1.2.0-dev.31+dirty"},
		{"a devel build", "", module("(devel)"), Dev},
		{"an empty module version", "", module(""), Dev},
		{"no build info", Dev, none, Dev},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Build(tc.stamp, tc.read); got != tc.want {
				t.Errorf("Build(%q) = %q, want %q", tc.stamp, got, tc.want)
			}
		})
	}
}

func TestWire(t *testing.T) {
	for _, tc := range []struct{ build, want string }{
		{"v1.2.0", "v1.2.0"},
		{"v1.2.0-dev.31", "v1.2.0-dev.31"},
		{"v1.2.0-dev.31.0.20261105101010-0123456789ab", Dev},
		{"v1.2.0-dev.31+dirty", Dev},
		{Dev, Dev},
	} {
		t.Run(tc.build, func(t *testing.T) {
			if got := Wire(tc.build); got != tc.want {
				t.Errorf("Wire(%q) = %q, want %q", tc.build, got, tc.want)
			}
		})
	}
}
