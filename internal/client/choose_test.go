package client

import (
	"errors"
	"strings"
	"testing"

	"github.com/cplieger/grelot/internal/provider"
)

func chooseSet(t *testing.T) *provider.Set {
	t.Helper()
	return providerSet(t,
		"alpha", "name: alpha\ndetect: {env: [ALPHA_ID]}\nsession: {env: ALPHA_ID}\n",
		"beta", "name: beta\ndetect: {env: [BETA_ID]}\nsession: {env: BETA_ID}\n",
		"silent", "name: silent\ndetect: {env: [SILENT_HOME]}\n",
		"broken", "name: broken\ncolour: red\n")
}

func TestChoose(t *testing.T) {
	set := chooseSet(t)
	cases := []struct {
		name, flag, config string
		lookup             func(string) (string, bool)
		want               string
	}{
		{"detection", "", "", env("ALPHA_ID", "a1"), "alpha"},
		{"the config's provider wins over detection", "", "beta", env("ALPHA_ID", "a1"), "beta"},
		{"the flag wins over the config", "alpha", "beta", env("BETA_ID", "b1"), "alpha"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := Choose(set, c.flag, &Config{Provider: c.config}, Shell{Lookup: c.lookup})
			if err != nil {
				t.Fatalf("Choose: %v", err)
			}
			if p.Name != c.want {
				t.Errorf("Choose = %s, want %s", p.Name, c.want)
			}
		})
	}
}

func TestChoose_noMatchIsTheDetectLine(t *testing.T) {
	_, err := Choose(chooseSet(t), "", &Config{}, Shell{Lookup: env("ALPHA_ID", "a", "BETA_ID", "b")})
	if _, ok := errors.AsType[*provider.DetectError](err); !ok {
		t.Fatalf("err = %v, want a *provider.DetectError", err)
	}
	for _, want := range []string{"matched alpha, beta", "provider broken invalid", "--provider <name>", "provider: <name>"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to name %q", err, want)
		}
	}
}

func TestSession(t *testing.T) {
	set := chooseSet(t)
	alpha, _ := set.Get("alpha")
	silent, _ := set.Get("silent")
	got, err := Session(alpha, env("ALPHA_ID", "s-1"))
	if err != nil || got != "alpha:s-1" {
		t.Errorf("Session(alpha) = %q, %v; want alpha:s-1", got, err)
	}
	for name, c := range map[string]struct {
		p      *provider.Provider
		lookup func(string) (string, bool)
	}{
		"unset variable":      {alpha, env()},
		"empty variable":      {alpha, env("ALPHA_ID", "")},
		"no session variable": {silent, env("SILENT_HOME", "/h")},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Session(c.p, c.lookup)
			var ns *provider.NoSessionError
			if got != "" || !errors.As(err, &ns) {
				t.Errorf("Session = %q, %v; want no session and a *provider.NoSessionError", got, err)
			}
		})
	}
}

func TestProviderNote(t *testing.T) {
	set := chooseSet(t)
	cases := []struct {
		name, flag, config string
		lookup             func(string) (string, bool)
		want               string
	}{
		{"no flag", "", "", env("ALPHA_ID", "a"), ""},
		{"no flag with the config's provider", "", "beta", env("ALPHA_ID", "a"), ""},
		{"the flag matches detection", "alpha", "", env("ALPHA_ID", "a"), ""},
		{"the flag matches the config", "beta", "beta", env("ALPHA_ID", "a"), ""},
		{
			"the flag differs from detection", "beta", "", env("ALPHA_ID", "a"),
			"grelot: this shell gives provider alpha, not beta; add provider: beta to the client config, or every grelot command here without --provider beta sends another session",
		},
		{
			"the flag differs from the config", "beta", "alpha", env("BETA_ID", "b"),
			"grelot: this shell gives provider alpha from the client config, not beta; add provider: beta to the client config, or every grelot command here without --provider beta sends another session",
		},
		{
			"detection finds none", "beta", "", env(),
			"grelot: this shell gives no single provider, not beta; add provider: beta to the client config, or every grelot command here without --provider beta sends another session",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ProviderNote(set, c.flag, &Config{Provider: c.config}, Shell{Lookup: c.lookup}); got != c.want {
				t.Errorf("ProviderNote =\n%q\nwant\n%q", got, c.want)
			}
		})
	}
}
