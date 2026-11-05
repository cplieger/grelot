package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// pushFile is a provider file with one http door whose header reads the
// secret token_name.
func pushFile(name, match string) string {
	return "name: " + name + "\ndoors:\n  - name: push\n    layer: 1\n    transport: http\n" +
		"    url: 'https://receiver.example/notify/{{path .Session}}'\n    body: '{\"line\": {{json .Line}}}'\n" +
		"    headers: {Authorization: 'Bearer {{secret \"door_token\"}}'}\n" + match
}

func loadServer(t *testing.T, fsys fstest.MapFS) (*ServerDoors, []*LoadError) {
	t.Helper()
	doors, skipped, err := LoadServerDir(fsys)
	if err != nil {
		t.Fatalf("LoadServerDir: %v", err)
	}
	return doors, skipped
}

func skippedNames(skipped []*LoadError) []string {
	var names []string
	for _, le := range skipped {
		names = append(names, le.Provider)
	}
	return names
}

func doorFiles(doors *ServerDoors) []string {
	var names []string
	for _, d := range doors.All() {
		names = append(names, d.File+"/"+d.Door.Name)
	}
	return names
}

func TestLoadServerDir(t *testing.T) {
	t.Setenv("SECRET_DOOR_TOKEN", "tok-value-1")
	t.Run("only http doors, the full schema decoded, extends unresolved", func(t *testing.T) {
		watcherOnly := "name: watcher\ndetect: {env: [W_ID]}\nsession: {env: W_ID}\n" +
			"doors:\n  - {name: out, layer: 2, transport: stdout}\nhooks:\n  stop: {event: Stop, outputs: {block: {text: '{{.Reason}}', exit: 2}}}\n"
		child := "name: child\nextends: nowhere\n" + strings.TrimPrefix(pushFile("child", ""), "name: child\n")
		mixed := pushFile("mixed", "    match: {host: other}\n") + "  - {name: queue, layer: 2, transport: exec, argv: [notify, '{{.Line}}']}\n"
		doors, skipped := loadServer(t, files("watcher", watcherOnly, "child", child, "mixed", mixed))
		if len(skipped) != 0 {
			t.Fatalf("skipped = %v, want none", skipped)
		}
		if got, want := doorFiles(doors), []string{"child/push", "mixed/push"}; !slices.Equal(got, want) {
			t.Errorf("doors = %v, want %v", got, want)
		}
	})
	t.Run("rendered with the resolved secret", func(t *testing.T) {
		doors, _ := loadServer(t, files("host", pushFile("host", "")))
		d, ok := doors.For("host", "any-label")
		if !ok {
			t.Fatal("For(host) found no door")
		}
		req, err := d.Door.RenderRequest(&sampleData)
		if err != nil {
			t.Fatalf("RenderRequest: %v", err)
		}
		if got := req.Header["Authorization"]; got != "Bearer tok-value-1" {
			t.Errorf("Authorization = %q, want the SECRET_DOOR_TOKEN value", got)
		}
		if got := doors.Secrets(); !slices.Equal(got, []string{"tok-value-1"}) {
			t.Errorf("Secrets = %v, want the one resolved value", got)
		}
	})
	t.Run("a missing folder holds no doors", func(t *testing.T) {
		doors, skipped, err := LoadServerDir(os.DirFS(filepath.Join(t.TempDir(), "absent")))
		if err != nil || len(skipped) != 0 || len(doors.All()) != 0 {
			t.Errorf("got %v doors, skipped %v, err %v; want an empty set", doors.All(), skipped, err)
		}
	})
}

func TestLoadServerDirMatch(t *testing.T) {
	t.Setenv("SECRET_DOOR_TOKEN", "tok")
	doors, skipped := loadServer(t, files(
		"own", pushFile("own", ""),
		"shared", pushFile("shared", "    match: {provider: inner-cli, host: container-label}\n"),
	))
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v, want none", skipped)
	}
	cases := []struct {
		name, provider, host, want string
	}{
		{"the file's own provider on any host", "own", "laptop", "own"},
		{"another provider on the door's host label", "inner-cli", "container-label", "shared"},
		{"that provider on another host label", "inner-cli", "laptop", ""},
		{"the serving file's own provider", "shared", "container-label", ""},
		{"an unknown provider", "nobody", "container-label", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, ok := doors.For(c.provider, c.host)
			if got := map[bool]string{true: d.File}[ok]; got != c.want {
				t.Errorf("For(%s, %s) = %q, want %q", c.provider, c.host, got, c.want)
			}
		})
	}
}

func TestLoadServerDirConflict(t *testing.T) {
	t.Setenv("SECRET_DOOR_TOKEN", "tok")
	cases := []struct {
		name         string
		first, later string
		conflict     bool
	}{
		{"same provider, both hosts left out", "", "    match: {provider: a-first}\n", true},
		{"same provider, the later host left out", "    match: {host: h1}\n", "    match: {provider: a-first}\n", true},
		{"same provider, the earlier host left out", "", "    match: {provider: a-first, host: h1}\n", true},
		{"same provider and host", "    match: {host: h1}\n", "    match: {provider: a-first, host: h1}\n", true},
		{"same provider, different hosts", "    match: {host: h1}\n", "    match: {provider: a-first, host: h2}\n", false},
		{"different providers", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doors, skipped := loadServer(t, files("a-first", pushFile("a-first", c.first), "b-later", pushFile("b-later", c.later)))
			if !c.conflict {
				if len(skipped) != 0 || len(doors.All()) != 2 {
					t.Errorf("doors %v, skipped %v; want both loaded", doorFiles(doors), skipped)
				}
				return
			}
			if got := doorFiles(doors); !slices.Equal(got, []string{"a-first/push"}) {
				t.Errorf("doors = %v, want only the earlier file's", got)
			}
			if len(skipped) != 1 || skipped[0].Provider != "b-later" || !strings.Contains(skipped[0].Error(), "a-first") {
				t.Errorf("skipped = %v, want b-later naming a-first", skipped)
			}
		})
	}
	t.Run("the first file by name wins", func(t *testing.T) {
		fsys := fstest.MapFS{}
		var later []string
		for i := range 40 {
			name := fmt.Sprintf("p%02d", i)
			fsys[name+".yaml"] = &fstest.MapFile{Data: []byte(pushFile(name, "    match: {provider: shared}\n"))}
			if i > 0 {
				later = append(later, name)
			}
		}
		doors, skipped := loadServer(t, fsys)
		if got := doorFiles(doors); !slices.Equal(got, []string{"p00/push"}) {
			t.Errorf("doors = %v, want p00's", got)
		}
		if got := skippedNames(skipped); !slices.Equal(got, later) {
			t.Errorf("skipped = %v, want %v", got, later)
		}
	})
	t.Run("two doors of one file", func(t *testing.T) {
		twice := pushFile("twin", "") + "  - name: again\n    layer: 1\n    transport: http\n    url: 'https://receiver.example/x'\n    body: '{}'\n"
		doors, skipped := loadServer(t, files("twin", twice))
		if len(doors.All()) != 0 || !slices.Equal(skippedNames(skipped), []string{"twin"}) {
			t.Errorf("doors %v, skipped %v; want twin skipped whole", doorFiles(doors), skipped)
		}
	})
}

func TestLoadServerDirSkips(t *testing.T) {
	t.Setenv("SECRET_DOOR_TOKEN", "tok-value-1")
	const value = "tok-value-1"
	badName := strings.Replace(pushFile("bad-name", ""), "door_token", "door-token", 1)
	unset := strings.Replace(pushFile("unset", ""), "door_token", "unset_token", 1)
	cases := []struct {
		name, file, want string
	}{
		{"an unknown key", "name: broken\ncolour: red\ndoors: []\n", "colour"},
		{"a secret name outside [a-z0-9_]+", badName, "headers.Authorization"},
		{"an unset secret", unset, "SECRET_UNSET_TOKEN is not set"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			name := strings.TrimPrefix(strings.SplitN(c.file, "\n", 2)[0], "name: ")
			doors, skipped := loadServer(t, files(name, c.file, "good", pushFile("good", "    match: {host: elsewhere}\n")))
			if got := doorFiles(doors); !slices.Equal(got, []string{"good/push"}) {
				t.Errorf("doors = %v, want only good's", got)
			}
			if len(skipped) != 1 {
				t.Fatalf("skipped = %v, want one", skipped)
			}
			msg := skipped[0].Error()
			if skipped[0].Provider != name || !strings.Contains(msg, c.want) || strings.Contains(msg, value) {
				t.Errorf("skipped = %q, want %s naming %q and no secret value", msg, name, c.want)
			}
		})
	}
}

func TestLoadServerDirSecretFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cases := []struct {
		name, path, want string
	}{
		{"read from the _FILE path", write("token", "file-value\n"), ""},
		{"a blank file", write("blank", " \n"), "names a blank file"},
		{"a path with a .. component", dir + "/../" + filepath.Base(dir) + "/token", "clean path"},
		{"a file that is not there", filepath.Join(dir, "absent"), "cannot be read"},
		{"the secret itself set as the path", "file-value", "cannot be read"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("SECRET_DOOR_TOKEN", "env-value")
			t.Setenv("SECRET_DOOR_TOKEN_FILE", c.path)
			doors, skipped := loadServer(t, files("host", pushFile("host", "")))
			if c.want == "" {
				if len(skipped) != 0 || !slices.Equal(doors.Secrets(), []string{"file-value"}) {
					t.Errorf("secrets %v, skipped %v; want the file's value", doors.Secrets(), skipped)
				}
				return
			}
			if len(skipped) != 1 {
				t.Fatalf("skipped = %v, want one", skipped)
			}
			msg := skipped[0].Error()
			if !strings.Contains(msg, "SECRET_DOOR_TOKEN_FILE") || !strings.Contains(msg, c.want) ||
				strings.Contains(msg, "file-value") || strings.Contains(msg, dir) {
				t.Errorf("skipped = %q, want the _FILE key and %q, no value and no path", msg, c.want)
			}
		})
	}
}
