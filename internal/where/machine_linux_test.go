package where

import (
	"io/fs"
	"testing"
	"testing/fstest"
)

const (
	dmiUUID    = "4c4c4544-0042-3510-8051-b4c04f334a32"
	dmiValue   = "dmi:fc59f36df66249a0e8e6a447823ec7d1"        // gitleaks:allow
	machineID  = "6b1f0c7e2d9a4f3b8c5e1a7d0f2b4c6e\n"          // gitleaks:allow
	machineIDV = "machine-id:5275acb6a10e2472412934258d8896fe" // gitleaks:allow
)

// deniedFS answers fs.ErrPermission for one path, as a non-root process
// reading the root-only product_uuid gets.
type deniedFS struct {
	fs.FS
	denied string
}

func (d deniedFS) Open(name string) (fs.File, error) {
	if name == d.denied {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
	}
	return d.FS.Open(name)
}

func tree(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for name, data := range files {
		m[name] = &fstest.MapFile{Data: []byte(data)}
	}
	return m
}

func TestLinuxMachine(t *testing.T) {
	const uuid = "sys/class/dmi/id/product_uuid"
	tests := []struct {
		sys  fs.FS
		name string
		want string
	}{
		{name: "dmi first", sys: tree(map[string]string{uuid: dmiUUID + "\n", "etc/machine-id": machineID}), want: dmiValue},
		{name: "machine-id without dmi", sys: tree(map[string]string{"etc/machine-id": machineID}), want: machineIDV},
		{name: "dmi in a docker container", sys: tree(map[string]string{uuid: dmiUUID, ".dockerenv": "", "etc/machine-id": machineID}), want: dmiValue},
		{name: "docker container skips machine-id", sys: tree(map[string]string{".dockerenv": "", "etc/machine-id": machineID}), want: ""},
		{name: "podman container skips machine-id", sys: tree(map[string]string{"run/.containerenv": "", "etc/machine-id": machineID}), want: ""},
		{
			name: "unreadable dmi outside a container",
			sys:  deniedFS{tree(map[string]string{uuid: dmiUUID, "etc/machine-id": machineID}), uuid},
			want: machineIDV,
		},
		{
			name: "unreadable dmi inside a container",
			sys:  deniedFS{tree(map[string]string{uuid: dmiUUID, "run/.containerenv": "", "etc/machine-id": machineID}), uuid},
			want: "",
		},
		{name: "placeholder falls through", sys: tree(map[string]string{uuid: "03000200-0400-0500-0006-000700080009", "etc/machine-id": machineID}), want: machineIDV},
		{name: "placeholder in a container", sys: tree(map[string]string{uuid: "00000000-0000-0000-0000-000000000000", ".dockerenv": ""}), want: ""},
		{name: "forty digits fall through", sys: tree(map[string]string{uuid: "00000000-2093bfe3-e53b-4fc3-9cb1-9217ea6228c7", "etc/machine-id": machineID}), want: machineIDV},
		{name: "non-hex falls through", sys: tree(map[string]string{uuid: "4c4c4544-0042-3510-8051-b4c04f334a3z", "etc/machine-id": machineID}), want: machineIDV},
		{name: "nothing", sys: tree(map[string]string{}), want: ""},
		{name: "placeholder machine-id", sys: tree(map[string]string{"etc/machine-id": "00000000000000000000000000000000\n"}), want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := linuxMachine(tt.sys); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
