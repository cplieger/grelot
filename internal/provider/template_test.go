package provider

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// httpDoor is a file with one http door; extra is spliced into the door.
func httpDoor(extra string) string {
	return "name: example\ndoors:\n  - name: push\n    layer: 1\n    transport: http\n" + extra
}

func mustLoad(t *testing.T, file string) *Provider {
	t.Helper()
	p, err := Load("example", []byte(file))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return p
}

// refused asserts Load refuses file with an error naming key and holding
// every one of want.
func refused(t *testing.T, file, key string, want ...string) {
	t.Helper()
	p, err := Load("example", []byte(file))
	if err == nil {
		t.Fatalf("Load accepted the file: %+v", p)
	}
	le, ok := errors.AsType[*LoadError](err)
	if !ok {
		t.Fatalf("error %T %v is not a LoadError", err, err)
	}
	if le.Key != key {
		t.Errorf("key = %q, want %q (error %v)", le.Key, key, err)
	}
	for _, w := range want {
		if !strings.Contains(le.Problem, w) {
			t.Errorf("problem %q lacks %q", le.Problem, w)
		}
	}
}

func TestTemplateJSON(t *testing.T) {
	p := mustLoad(t, httpDoor("    url: 'https://receiver/notify'\n    body: '{\"text\":{{json .Line}},\"seq\":{{json .Seq}}}'\n"))
	line := "a \"quoted\" word\nsecond line\ttab\x01\x1f\\ end \u2028"
	req, err := p.Doors[0].RenderRequest(&Data{Line: line, Seq: 7})
	if err != nil {
		t.Fatalf("RenderRequest: %v", err)
	}
	var got struct {
		Text string `json:"text"`
		Seq  uint64 `json:"seq"`
	}
	if err := json.Unmarshal([]byte(req.Body), &got); err != nil {
		t.Fatalf("body %q is not JSON: %v", req.Body, err)
	}
	if got.Text != line || got.Seq != 7 {
		t.Errorf("decoded %+v, want text %q and seq 7", got, line)
	}
	if strings.ContainsAny(req.Body, "\n\x01\x1f") {
		t.Errorf("body %q carries a raw control character", req.Body)
	}
}

func TestTemplatePath(t *testing.T) {
	p := mustLoad(t, httpDoor("    url: 'http://receiver:9847/api/sessions/{{path .Session}}/notify?id={{path .Identity}}'\n    body: '{}'\n"))
	req, err := p.Doors[0].RenderRequest(&Data{Session: "a/b?c%d", Identity: "x"})
	if err != nil {
		t.Fatalf("RenderRequest: %v", err)
	}
	// RFC 3986: "/", "?" and "%" are not unreserved, so each is
	// percent-encoded inside one path segment.
	if want := "http://receiver:9847/api/sessions/a%2Fb%3Fc%25d/notify?id=x"; req.URL != want {
		t.Errorf("url = %q, want %q", req.URL, want)
	}
}

func TestTemplateURLLiteralAuthority(t *testing.T) {
	cases := []struct{ name, url, want string }{
		{"action in the scheme", "{{.Session}}://receiver/x", "http://"},
		{"action in the host", "http://{{.Session}}/x", "path and the query"},
		{"action inside the host", "http://recv{{.Session}}/x", "path and the query"},
		{"action in the port", "http://receiver:{{.Seq}}/x", "path and the query"},
		{"action right after the host", "http://receiver{{path .Session}}", "path and the query"},
		{"another scheme", "ftp://receiver/{{.Session}}", "http://"},
		{"credentials", "http://user:pw@receiver/x", "credentials"},
		{"no host", "http:///x", "host"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			refused(t, httpDoor("    url: '"+c.url+"'\n    body: '{}'\n"), "doors[0].url", c.want)
		})
	}
	t.Run("actions in the path and the query", func(t *testing.T) {
		mustLoad(t, httpDoor("    url: 'https://receiver:8443/s/{{path .Session}}?seq={{.Seq}}'\n    body: '{}'\n"))
		mustLoad(t, httpDoor("    url: 'https://receiver?seq={{.Seq}}'\n    body: '{}'\n"))
	})
}

func TestTemplateSecret(t *testing.T) {
	const header = "    url: 'https://receiver/x'\n    body: '{}'\n    headers:\n      Authorization: 'Bearer {{secret \"door_token\"}}'\n      X-Other: '{{secret \"door_token\"}}{{secret \"other_key\"}}'\n      X-Plain: plain\n"
	t.Run("names", func(t *testing.T) {
		p := mustLoad(t, httpDoor(header))
		if got, want := p.Doors[0].secretNames(), []string{"door_token", "other_key"}; !slices.Equal(got, want) {
			t.Errorf("secretNames = %v, want %v", got, want)
		}
	})
	t.Run("unresolved refuses to render", func(t *testing.T) {
		p := mustLoad(t, httpDoor(header))
		if _, err := p.Doors[0].RenderRequest(&sampleData); err == nil {
			t.Error("rendered a request with an unresolved secret")
		}
	})
	t.Run("resolved", func(t *testing.T) {
		p := mustLoad(t, httpDoor(header))
		values := map[string]string{"door_token": "tok-1", "other_key": "k2"}
		err := p.Doors[0].ResolveSecrets(func(name string) (string, error) { return values[name], nil })
		if err != nil {
			t.Fatalf("ResolveSecrets: %v", err)
		}
		req, err := p.Doors[0].RenderRequest(&sampleData)
		if err != nil {
			t.Fatalf("RenderRequest: %v", err)
		}
		want := map[string]string{"Authorization": "Bearer tok-1", "X-Other": "tok-1k2", "X-Plain": "plain"}
		for k, v := range want {
			if req.Header[k] != v {
				t.Errorf("header %s = %q, want %q", k, req.Header[k], v)
			}
		}
	})
	t.Run("lookup failure names the secret", func(t *testing.T) {
		p := mustLoad(t, httpDoor(header))
		err := p.Doors[0].ResolveSecrets(func(name string) (string, error) {
			if name == "other_key" {
				return "", errors.New("unset")
			}
			return "tok-1", nil
		})
		if err == nil || !strings.Contains(err.Error(), "other_key") || strings.Contains(err.Error(), "tok-1") {
			t.Errorf("error = %v, want one naming other_key and no value", err)
		}
	})
	outside := []struct{ name, file, key string }{
		{"body", httpDoor("    url: 'https://receiver/x'\n    body: '{{secret \"door_token\"}}'\n"), "doors[0].body"},
		{"url", httpDoor("    url: 'https://receiver/{{secret \"door_token\"}}'\n    body: '{}'\n"), "doors[0].url"},
		{"exec argv", "name: example\ndoors:\n  - {name: run, layer: 2, transport: exec, argv: [notify, '{{secret \"door_token\"}}']}\n", "doors[0].argv[1]"},
		{"line", "name: example\ndoors:\n  - {name: out, layer: 2, transport: stdout, line: '{{secret \"door_token\"}}'}\n", "doors[0].line"},
		{"end", "name: example\ndoors:\n  - {name: out, layer: 2, transport: exit, end: '{{secret \"door_token\"}}'}\n", "doors[0].end"},
		{"idle", "name: example\ndoors:\n  - {name: out, layer: 2, transport: exit, hold: 45s, idle: '{{secret \"door_token\"}}'}\n", "doors[0].idle"},
		{"hook output", "name: example\nhooks:\n  stop: {event: Stop, outputs: {block: {text: '{{secret \"door_token\"}}'}}}\n", "hooks.stop.outputs.block.text"},
	}
	for _, c := range outside {
		t.Run("refused in "+c.name, func(t *testing.T) {
			refused(t, c.file, c.key, "only in an http door's header values")
		})
	}
	bad := []struct{ name, value string }{
		{"a name with a hyphen", `{{secret "door-token"}}`},
		{"an upper-case name", `{{secret "DOOR_TOKEN"}}`},
		{"an empty name", `{{secret ""}}`},
		{"a field as the name", `{{secret .Session}}`},
		{"two names", `{{secret "a" "b"}}`},
		{"a piped name", `{{"door_token" | secret}}`},
		{"secret after a pipe", `{{"x" | secret "door_token"}}`},
		{"secret as an argument", `{{print secret}}`},
	}
	for _, c := range bad {
		t.Run("refused with "+c.name, func(t *testing.T) {
			file := httpDoor("    url: 'https://receiver/x'\n    body: '{}'\n    headers: {Authorization: '" + strings.ReplaceAll(c.value, "'", "''") + "'}\n")
			refused(t, file, "doors[0].headers.Authorization", "secret")
		})
	}
}

func TestTemplateHeaderValue(t *testing.T) {
	header := func(value string) string {
		return httpDoor("    url: 'https://receiver/x'\n    body: '{}'\n    headers: {X-Grelot: " + value + "}\n")
	}
	refusedCases := []struct{ name, value string }{
		{"a literal line break", `"a\r\nX-Injected: 1"`},
		{"a literal DEL", `"a\x7fb"`},
		{"a field that can hold a line break", `'{{.Reason}}'`},
	}
	for _, c := range refusedCases {
		t.Run("refused with "+c.name, func(t *testing.T) {
			refused(t, header(c.value), "doors[0].headers.X-Grelot", "a header value cannot carry")
		})
	}
	t.Run("a value that renders a line break is not rendered", func(t *testing.T) {
		p := mustLoad(t, header(`'{{.Session}}'`))
		_, err := p.Doors[0].RenderRequest(&Data{Session: "s-1\r\nX-Injected: 1"})
		if err == nil || !strings.Contains(err.Error(), "X-Grelot") || strings.Contains(err.Error(), "s-1") {
			t.Errorf("RenderRequest = %v, want an error naming the header and not the value", err)
		}
	})
	t.Run("a tab, a field escaped as JSON and a non-ASCII letter load", func(t *testing.T) {
		p := mustLoad(t, header(`"a\tb {{json .Reason}} \u00e9"`))
		req, err := p.Doors[0].RenderRequest(&Data{Reason: "x\ny"})
		if err != nil {
			t.Fatalf("RenderRequest: %v", err)
		}
		if got, want := req.Header["X-Grelot"], "a\tb \"x\\ny\" \u00e9"; got != want {
			t.Errorf("header = %q, want %q", got, want)
		}
	})
}

func TestTemplateUnknownData(t *testing.T) {
	cases := []struct{ name, text, want string }{
		{"unknown field", `{{.Bogus}}`, ".Bogus is not template data"},
		{"unknown field from the root", `{{$.Bogus}}`, ".Bogus is not template data"},
		{"nested field", `{{.Line.Len}}`, "no nested fields"},
		{"hook session outside the glob", `{{.HookSession}}`, "only in session.record.glob"},
		{"unknown function", `{{shell .Line}}`, "not a valid template"},
		{"a field in a branch the sample skips", `{{if eq .Seq 0}}{{.Bogus}}{{end}}`, ".Bogus is not template data"},
		{"the whole data", `{{.}}`, "one field"},
		{"the whole data as JSON", `{{json .}}`, "one field"},
		{"the whole data from the root", `{{json $}}`, "one field"},
		{"the whole data inside a with", `{{with .Line}}{{$}}{{end}}`, "one field"},
		{"the whole data in an else", `{{with .Context}}x{{else}}{{.}}{{end}}`, "one field"},
		{"a field read from the value a with binds", `{{with .Line}}{{.Seq}}{{end}}`, "$.Line"},
		{"a whole-data variable", `{{$d := .}}{{json $d}}`, "one field"},
	}
	for _, c := range cases {
		t.Run(c.name+" in a body", func(t *testing.T) {
			refused(t, httpDoor("    url: 'https://receiver/x'\n    body: '"+c.text+"'\n"), "doors[0].body", c.want)
		})
		t.Run(c.name+" in an output", func(t *testing.T) {
			refused(t, "name: example\nhooks:\n  prompt: {event: P, outputs: {context: {text: '"+c.text+"'}}}\n", "hooks.prompt.outputs.context.text", c.want)
		})
	}
	t.Run("the dot a with rebinds to one field", func(t *testing.T) {
		p := mustLoad(t, httpDoor("    url: 'https://receiver/x'\n    body: '{{with .Context}}{\"c\":{{json .}}}{{else}}{}{{end}}'\n"))
		req, err := p.Doors[0].RenderRequest(&Data{Context: "ctx", DedupeKey: "never-rendered"})
		if err != nil {
			t.Fatalf("RenderRequest: %v", err)
		}
		if req.Body != `{"c":"ctx"}` {
			t.Errorf("body = %q, want the context alone", req.Body)
		}
		q := mustLoad(t, httpDoor("    url: 'https://receiver/x'\n    body: '{{with .Context}}{{json $.Seq}}{{end}}'\n"))
		if req, err := q.Doors[0].RenderRequest(&Data{Context: "ctx", Seq: 9}); err != nil || req.Body != "9" {
			t.Errorf("body = %q, %v; want the root's Seq through $", req.Body, err)
		}
	})
	t.Run("an error the sample render finds", func(t *testing.T) {
		refused(t, "name: example\ndoors:\n  - {name: out, layer: 2, transport: stdout, line: '{{index .Line 1000}}'}\n", "doors[0].line", "sample data")
	})
}

func TestTemplateErrorQuotesNoText(t *testing.T) {
	_, err := Load("example", []byte(httpDoor("    url: 'https://receiver/x'\n    body: '{}'\n    headers: {Authorization: 'Bearer hunter2 {{'}\n")))
	if err == nil {
		t.Fatal("Load accepted an unclosed action")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error %q quotes the header's text", err)
	}
}

func TestTemplateDedupeWindow(t *testing.T) {
	const keyed = "    url: 'https://receiver/x'\n    body: '{\"key\":{{json .DedupeKey}}}'\n"
	t.Run("required when a template renders the key", func(t *testing.T) {
		refused(t, httpDoor(keyed), "doors[0].dedupe_window", "required", ".DedupeKey")
	})
	t.Run("required for a key in a header", func(t *testing.T) {
		refused(t, httpDoor("    url: 'https://receiver/x'\n    body: '{}'\n    headers: {Idempotency-Key: '{{.DedupeKey}}'}\n"), "doors[0].dedupe_window", "required")
	})
	t.Run("60s accepted", func(t *testing.T) {
		mustLoad(t, httpDoor(keyed+"    dedupe_window: 60s\n"))
	})
	t.Run("not required without the key", func(t *testing.T) {
		mustLoad(t, httpDoor("    url: 'https://receiver/x'\n    body: '{\"text\":{{json .Line}}}'\n"))
	})
}

func TestTemplateGlob(t *testing.T) {
	record := func(glob string) string {
		return "name: example\nsession:\n  record: {dir_default: ~/.example, parent: parentId, reason: why, glob: '" + glob + "'}\n"
	}
	p := mustLoad(t, record("sessions/*/{{.HookSession}}/session.json"))
	t.Run("one path component", func(t *testing.T) {
		got, err := p.Session.Record.RenderGlob("4f1c-9e2a")
		if err != nil || got != "sessions/*/4f1c-9e2a/session.json" {
			t.Errorf("RenderGlob = %q, %v", got, err)
		}
	})
	for _, s := range []string{"", ".", "..", "a/b", `a\b`, "../x", "*", "a?", "[ab]", "a\x00b"} {
		t.Run("refuses "+strings.ReplaceAll(s, "/", "slash"), func(t *testing.T) {
			if got, err := p.Session.Record.RenderGlob(s); err == nil {
				t.Errorf("RenderGlob(%q) = %q, want an error", s, got)
			}
		})
	}
	t.Run("a glob without the hook session", func(t *testing.T) {
		refused(t, record("sessions/*/session.json"), "session.record.glob", "{{.HookSession}}")
	})
	t.Run("door data in the glob", func(t *testing.T) {
		refused(t, record("{{.Session}}/{{.HookSession}}.json"), "session.record.glob", ".Session is not template data")
	})
	t.Run("an absolute glob", func(t *testing.T) {
		refused(t, record("/etc/{{.HookSession}}"), "session.record.glob", "relative")
	})
	t.Run("a glob leaving the directory", func(t *testing.T) {
		refused(t, record("../{{.HookSession}}"), "session.record.glob", "relative")
	})
	t.Run("a malformed pattern", func(t *testing.T) {
		refused(t, record("[{{.HookSession}}"), "session.record.glob", "glob pattern")
	})
}

func TestTemplateRender(t *testing.T) {
	p := mustLoad(t, full)
	byName := map[string]*Door{}
	for i := range p.Doors {
		byName[p.Doors[i].Name] = &p.Doors[i]
	}
	data := Data{Line: "[grelot:3] x", Session: "s-1", Identity: "me", DedupeKey: "id:2:1", Reason: "why", Context: "ctx"}
	t.Run("exec argv", func(t *testing.T) {
		got, err := byName["queue"].RenderArgv(&data)
		if want := []string{"example", "queue", "--thread", "s-1", "--message", "[grelot:3] x"}; err != nil || !slices.Equal(got, want) {
			t.Errorf("RenderArgv = %q, %v; want %q", got, err, want)
		}
	})
	t.Run("line, idle and end", func(t *testing.T) {
		d := byName["watcher"]
		line, err1 := d.RenderLine(&data)
		idle, err2 := d.RenderIdle(&data)
		end, err3 := d.RenderEnd(&Data{Line: "[grelot:end] stopped"})
		if err := errors.Join(err1, err2, err3); err != nil {
			t.Fatal(err)
		}
		if line != `{"line":"[grelot:3] x"}` || idle != `{"outcome":"idle"}` || end != `{"end":"[grelot:end] stopped"}` {
			t.Errorf("line %q, idle %q, end %q", line, idle, end)
		}
	})
	t.Run("hook output", func(t *testing.T) {
		got, err := p.Hooks.Prompt.Outputs.Block.Render(&Data{Reason: "stale \"signal\""})
		if want := `{"decision":"block","reason":"stale \"signal\""}`; err != nil || got != want {
			t.Errorf("Render = %q, %v; want %q", got, err, want)
		}
	})
	t.Run("default line", func(t *testing.T) {
		q := mustLoad(t, "name: example\ndoors:\n  - {name: out, layer: 2, transport: stdout}\n")
		got, err := q.Doors[0].RenderLine(&data)
		if err != nil || got != data.Line {
			t.Errorf("RenderLine = %q, %v; want the line", got, err)
		}
		if _, err := q.Doors[0].RenderIdle(&data); err == nil {
			t.Error("RenderIdle rendered for a door with no hold")
		}
	})
	t.Run("a render of another transport", func(t *testing.T) {
		if _, err := byName["notify"].RenderLine(&data); err == nil {
			t.Error("RenderLine rendered an http door")
		}
		if _, err := byName["queue"].RenderRequest(&data); err == nil {
			t.Error("RenderRequest rendered an exec door")
		}
		if _, err := byName["watcher"].RenderArgv(&data); err == nil {
			t.Error("RenderArgv rendered an exit door")
		}
	})
	t.Run("a door not loaded", func(t *testing.T) {
		d := Door{Transport: TransportStdout, Line: "{{.Line}}"}
		if _, err := d.RenderLine(&data); err == nil {
			t.Error("rendered a door Load never parsed")
		}
	})
}
