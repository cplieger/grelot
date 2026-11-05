package provider

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// checkFile is a provider with one of each template Check parses as JSON:
// an http body, a stdout or exit door's line, idle and end, and a hook output.
type checkFile struct {
	contentType, body, line, idle, end, block string
}

func (f checkFile) yaml() []byte {
	return fmt.Appendf(nil, `name: sample
doors:
  - name: notify
    layer: 1
    transport: http
    url: 'https://example.test/notify'
    headers: {Content-Type: '%s'}
    body: '%s'
  - name: watcher
    layer: 2
    transport: exit
    line: '%s'
    hold: 45s
    idle: '%s'
    end: '%s'
hooks:
  stop:
    event: Stop
    outputs:
      block: {text: '%s'}
`, f.contentType, f.body, f.line, f.idle, f.end, f.block)
}

// goodCheck runs every value placed in JSON through json, as the design's
// template rule asks.
var goodCheck = checkFile{
	contentType: "application/json",
	body:        `{"text":{{json .Line}},"session":{{json .Session}}}`,
	line:        `{"line":{{json .Line}}}`,
	idle:        `{"outcome":"idle"}`,
	end:         `{"end":{{json .Line}}}`,
	block:       `{"decision":"block","reason":{{json .Reason}}}`,
}

// problemKeys lists the key of every problem in err, a LoadError or a join
// of them.
func problemKeys(t *testing.T, err error) []string {
	t.Helper()
	errs := []error{err}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		errs = j.Unwrap()
	}
	var keys []string
	for _, e := range errs {
		le, ok := errors.AsType[*LoadError](e)
		if !ok {
			t.Fatalf("problem %v is not a *LoadError", e)
		}
		if le.Provider != "sample" {
			t.Errorf("problem %v names provider %q, want sample", e, le.Provider)
		}
		keys = append(keys, le.Key)
	}
	slices.Sort(keys)
	return keys
}

func TestCheck(t *testing.T) {
	t.Run("a file with every JSON value through json passes", func(t *testing.T) {
		if err := Check("sample", goodCheck.yaml()); err != nil {
			t.Errorf("Check: %v", err)
		}
	})
	t.Run("the schema example passes", func(t *testing.T) {
		if err := Check("example", []byte(full)); err != nil {
			t.Errorf("Check: %v", err)
		}
	})
	t.Run("plain text outputs are not parsed", func(t *testing.T) {
		f := goodCheck
		f.contentType = "text/plain"
		f.body = "text={{.Reason}}"
		f.line, f.end, f.block = "{{.Line}}", "stopped: {{.Line}}", "{{.Reason}}"
		f.idle = "idle"
		if err := Check("sample", f.yaml()); err != nil {
			t.Errorf("Check: %v", err)
		}
	})

	cases := []struct {
		edit func(*checkFile)
		name string
		key  string
	}{
		{
			name: "body with a value outside json", key: "doors[0].body",
			edit: func(f *checkFile) { f.body = `{"text":"{{.Line}}"}` },
		},
		{
			name: "JSON content type with a body that is not JSON", key: "doors[0].body",
			edit: func(f *checkFile) { f.body = "text={{json .Line}}" },
		},
		{
			name: "JSON content type read case-insensitively", key: "doors[0].body",
			edit: func(f *checkFile) { f.contentType, f.body = "Application/JSON; charset=utf-8", "text" },
		},
		{
			name: "a JSON array body", key: "doors[0].body",
			edit: func(f *checkFile) { f.body = `["{{.Session}}"]` },
		},
		{
			name: "door line", key: "doors[1].line",
			edit: func(f *checkFile) { f.line = `{"line":"{{.Line}}"}` },
		},
		{
			name: "door idle", key: "doors[1].idle",
			edit: func(f *checkFile) { f.idle = `{"outcome":idle}` },
		},
		{
			name: "door end", key: "doors[1].end",
			edit: func(f *checkFile) { f.end = `{"end":"{{.Line}}"}` },
		},
		{
			name: "hook output", key: "hooks.stop.outputs.block.text",
			edit: func(f *checkFile) { f.block = `{"reason":"{{.Reason}}"}` },
		},
		{
			name: "leading space before the brace", key: "hooks.stop.outputs.block.text",
			edit: func(f *checkFile) { f.block = ` {"reason":{{.Seq}},}` },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := goodCheck
			c.edit(&f)
			err := Check("sample", f.yaml())
			if err == nil {
				t.Fatal("Check passed, want a JSON problem")
			}
			if got := problemKeys(t, err); !slices.Equal(got, []string{c.key}) {
				t.Errorf("problem keys = %v, want [%s]", got, c.key)
			}
		})
	}

	t.Run("a header name read case-insensitively", func(t *testing.T) {
		f := goodCheck
		f.body = "text"
		text := strings.Replace(string(f.yaml()), "{Content-Type:", "{content-type:", 1)
		got := problemKeys(t, Check("sample", []byte(text)))
		if want := []string{"doors[0].body"}; !slices.Equal(got, want) {
			t.Errorf("problem keys = %v, want %v", got, want)
		}
	})
	t.Run("a door is named by its place in the file", func(t *testing.T) {
		// The watcher's end defaults to its line, so both fail.
		text := `name: sample
doors:
  - name: watcher
    layer: 2
    transport: stdout
    line: '{"line":"{{.Line}}"}'
  - name: notify
    layer: 1
    transport: http
    url: 'https://example.test/notify'
    body: '{"text":{{json .Line}}}'
`
		got := problemKeys(t, Check("sample", []byte(text)))
		if want := []string{"doors[0].end", "doors[0].line"}; !slices.Equal(got, want) {
			t.Errorf("problem keys = %v, want %v", got, want)
		}
	})
	t.Run("every JSON problem is reported", func(t *testing.T) {
		f := goodCheck
		f.body, f.block = `{"text":"{{.Line}}"}`, `{"reason":"{{.Reason}}"}`
		got := problemKeys(t, Check("sample", f.yaml()))
		if want := []string{"doors[0].body", "hooks.stop.outputs.block.text"}; !slices.Equal(got, want) {
			t.Errorf("problem keys = %v, want %v", got, want)
		}
	})
	t.Run("a problem never quotes the template", func(t *testing.T) {
		f := goodCheck
		f.body = `{"text":"literal-credential {{.Line}}"}`
		err := Check("sample", f.yaml())
		if err == nil || strings.Contains(err.Error(), "literal-credential") || strings.Contains(err.Error(), "{{") {
			t.Errorf("Check = %v, want a problem with no template text", err)
		}
	})
	t.Run("a load failure is the load error", func(t *testing.T) {
		err := Check("sample", append(goodCheck.yaml(), "bogus: 1\n"...))
		le, ok := errors.AsType[*LoadError](err)
		if !ok || le.Provider != "sample" {
			t.Errorf("Check = %v, want the provider's *LoadError", err)
		}
	})
}
