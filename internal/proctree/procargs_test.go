package proctree

import (
	"encoding/binary"
	"slices"
	"testing"
)

// procargs2 lays a buffer out as xnu's sysctl_procargsx does: argc, the
// executable path, NUL padding, the arguments, then the environment.
func procargs2(argc uint32, path string, pad int, rest string) []byte {
	b := binary.LittleEndian.AppendUint32(nil, argc)
	b = append(b, path...)
	b = append(b, make([]byte, pad)...)
	return append(b, rest...)
}

func TestParseProcargs2(t *testing.T) {
	tests := []struct {
		name string
		buf  []byte
		want []string
	}{
		{
			"arguments before the environment",
			procargs2(3, "/opt/homebrew/bin/agentcli", 5, "agentcli\x00serve-many\x00--listen\x00HOME=/Users/u\x00"),
			[]string{"agentcli", "serve-many", "--listen"},
		},
		{"one padding byte", procargs2(1, "/bin/zsh", 1, "-zsh\x00"), []string{"-zsh"}},
		{"fewer arguments than argc", procargs2(3, "/bin/zsh", 2, "zsh\x00-l\x00"), nil},
		{"an argument with no terminator", procargs2(1, "/bin/zsh", 2, "zsh"), nil},
		{"argc zero", procargs2(0, "/bin/zsh", 2, ""), nil},
		{"no path terminator", binary.LittleEndian.AppendUint32(nil, 1), nil},
		{"shorter than argc", []byte{1, 0}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseProcargs2(tt.buf); !slices.Equal(got, tt.want) || (got == nil) != (tt.want == nil) {
				t.Errorf("parseProcargs2 = %q, want %q", got, tt.want)
			}
		})
	}
}
