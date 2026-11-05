package proctree

import (
	"bytes"
	"encoding/binary"
)

// parseProcargs2 reads argv from a macOS kern.procargs2 buffer: a 32-bit
// argc in host order (little-endian on both Apple architectures), the
// executable path, NUL padding, then argc NUL-terminated arguments and the
// environment (xnu bsd/kern/kern_sysctl.c, sysctl_procargsx). A buffer
// holding fewer than argc arguments answers nil, as unreadable.
func parseProcargs2(b []byte) []string {
	if len(b) < 4 {
		return nil
	}
	argc := int(binary.LittleEndian.Uint32(b))
	rest := b[4:]
	i := bytes.IndexByte(rest, 0)
	if argc == 0 || i < 0 {
		return nil
	}
	rest = bytes.TrimLeft(rest[i:], "\x00")
	args := make([]string, 0, min(argc, 1024))
	for range argc {
		i := bytes.IndexByte(rest, 0)
		if i < 0 {
			return nil
		}
		args = append(args, string(rest[:i]))
		rest = rest[i+1:]
	}
	return args
}
