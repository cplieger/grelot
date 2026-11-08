package client

import (
	"slices"
	"strings"
	"testing"
)

const startLine = "start the watcher: run grelot watch in a background terminal"

func TestStatus_Line(t *testing.T) {
	cases := []struct {
		name string
		s    Status
		want string
	}{
		{
			"the server's line alone",
			Status{Server: "grelot: @tool-dev · 2 unread (1 wake) · wake: watcher", Start: startLine},
			"grelot: @tool-dev · 2 unread (1 wake) · wake: watcher",
		},
		{
			"wake down appends the door's start line",
			Status{Server: "grelot: @tool-dev · 0 unread · wake: down", Start: startLine},
			"grelot: @tool-dev · 0 unread · wake: down · " + startLine,
		},
		{
			"wake down with no layer-2 door",
			Status{Server: "grelot: @tool-dev · 0 unread · wake: down"},
			"grelot: @tool-dev · 0 unread · wake: down",
		},
		{
			"wake down before the lease note",
			Status{Server: "grelot: @tool-dev · 0 unread · wake: down · expires in 20 min", Start: startLine},
			"grelot: @tool-dev · 0 unread · wake: down · expires in 20 min · " + startLine,
		},
		{
			"another release base names the server's own tag",
			Status{Server: "grelot: @a · 0 unread · wake: watcher", Client: "v1.1.0", ServerVersion: "v1.2.0-dev.3"},
			"grelot: @a · 0 unread · wake: watcher · this client is v1.1.0 and the server v1.2.0-dev.3: install grelot v1.2.0-dev.3 with go install github.com/cplieger/grelot@v1.2.0-dev.3 or from https://github.com/cplieger/grelot/releases/tag/v1.2.0-dev.3",
		},
		{
			"an earlier dev build of the server's release",
			Status{Server: "grelot: @a · 0 unread · wake: watcher", Client: "v1.1.0-dev.24", ServerVersion: "v1.1.0-dev.25"},
			"grelot: @a · 0 unread · wake: watcher · this client is v1.1.0-dev.24 and the server v1.1.0-dev.25: install grelot v1.1.0-dev.25 with go install github.com/cplieger/grelot@v1.1.0-dev.25 or from https://github.com/cplieger/grelot/releases/tag/v1.1.0-dev.25",
		},
		{
			"a later dev build than the server says nothing",
			Status{Server: "grelot: @a · 0 unread · wake: watcher", Client: "v1.1.0-dev.26", ServerVersion: "v1.1.0-dev.25"},
			"grelot: @a · 0 unread · wake: watcher",
		},
		{
			"a promoted dev build is the same release",
			Status{Server: "grelot: @a · 0 unread · wake: watcher", Client: "v1.1.0", ServerVersion: "v1.1.0-dev.5"},
			"grelot: @a · 0 unread · wake: watcher",
		},
		{
			"a dev client says nothing",
			Status{Server: "grelot: @a · 0 unread · wake: watcher", Client: "dev", ServerVersion: "v1.2.0"},
			"grelot: @a · 0 unread · wake: watcher",
		},
		{
			"an unknown server version says nothing",
			Status{Server: "grelot: @a · 0 unread · wake: watcher", Client: "v1.1.0"},
			"grelot: @a · 0 unread · wake: watcher",
		},
		{
			"an invalid provider file",
			Status{Server: "grelot: @a · 0 unread · wake: watcher", Invalid: []string{"beta"}},
			"grelot: @a · 0 unread · wake: watcher · provider beta invalid: run grelot provider check on its file",
		},
		{
			"no server line with an invalid file",
			Status{Invalid: []string{"beta", "gamma"}},
			"grelot: provider beta invalid: run grelot provider check on its file · provider gamma invalid: run grelot provider check on its file",
		},
		{"nothing to say", Status{Client: "v1.1.0"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.s.Line(); got != c.want {
				t.Errorf("Line() =\n%q\nwant\n%q", got, c.want)
			}
		})
	}
}

func TestStatus_LineIsOneSafeLine(t *testing.T) {
	s := Status{Server: "grelot: @a\x1b[2J · 1 unread\n · wake: down", Start: "run\rgrelot\u202ewatch"}
	got := s.Line()
	if strings.ContainsAny(got, "\x1b\r\n\u202e") {
		t.Errorf("Line() = %q, want no escape, line break or bidi control", got)
	}
}

func TestFirstStart(t *testing.T) {
	set := providerSet(t, "alpha", `name: alpha
detect: {env: [ALPHA_ID]}
session: {env: ALPHA_ID}
doors:
  - {name: push, layer: 1, transport: http, url: "https://alpha.example/n", body: "{}", start: push start}
  - {name: watch, layer: 2, transport: stdout, start: watch start}
  - {name: queue, layer: 2, transport: exit, start: queue start}
`, "bare", "name: bare\ndetect: {env: [BARE_ID]}\nsession: {env: BARE_ID}\n")
	alpha, err := set.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	bare, err := set.Get("bare")
	if err != nil {
		t.Fatal(err)
	}
	if got := FirstStart(alpha); got != "watch start" {
		t.Errorf("FirstStart(alpha) = %q, want the first layer-2 door's", got)
	}
	if got := FirstStart(bare); got != "" {
		t.Errorf("FirstStart(bare) = %q, want none", got)
	}
	if got := FirstStart(nil); got != "" {
		t.Errorf("FirstStart(nil) = %q, want none", got)
	}
}

func TestInvalid(t *testing.T) {
	set := providerSet(t,
		"alpha", "name: alpha\ndetect: {env: [ALPHA_ID]}\nsession: {env: ALPHA_ID}\n",
		"beta", "name: beta\ncolour: red\n",
		"gamma", "name: gamma\nextends: beta\n")
	if got := Invalid(set); !slices.Equal(got, []string{"beta", "gamma"}) {
		t.Errorf("Invalid = %v, want [beta gamma]", got)
	}
}

func TestOlder(t *testing.T) {
	for _, tc := range []struct {
		client, server string
		want           bool
	}{
		{"v1.1.0", "v1.2.0", true},
		{"v1.9.0", "v1.10.0", true},
		{"v1.1.0-dev.24", "v1.1.0-dev.25", true},
		{"v1.1.0-dev.9", "v1.1.0-dev.10", true},
		{"v1.1.0-dev.30", "v1.2.0-dev.1", true},
		{"v1.1.0", "v1.1.0-dev.25", false},
		{"v1.1.0-dev.25", "v1.1.0", false},
		{"v1.1.0-dev.25", "v1.1.0-dev.25", false},
		{"v1.2.0", "v1.1.0", false},
		{"dev", "v1.2.0", false},
		{"v1.1.0", "dev", false},
		{"v1.1.0", "", false},
	} {
		t.Run(tc.client+" against "+tc.server, func(t *testing.T) {
			if got := older(tc.client, tc.server); got != tc.want {
				t.Errorf("older(%q, %q) = %v, want %v", tc.client, tc.server, got, tc.want)
			}
		})
	}
}
