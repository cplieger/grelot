package where

import (
	"context"
	"io/fs"
	"os"
)

// Machine is this machine's value for where.machine, or "" when no source
// names one.
func Machine(context.Context) string { return linuxMachine(os.DirFS("/")) }

// InContainer reports whether this process runs in a container.
func InContainer() bool { return inContainer(os.DirFS("/")) }

// linuxMachine reads the sources in order over sys, the filesystem root. A
// container's /etc/machine-id names its image, not the machine, so it is read
// only outside one.
func linuxMachine(sys fs.FS) string {
	if raw, err := readSmall(sys, "sys/class/dmi/id/product_uuid"); err == nil {
		if v := machineValue(KindDMI, raw); v != "" {
			return v
		}
	}
	if inContainer(sys) {
		return ""
	}
	if raw, err := readSmall(sys, "etc/machine-id"); err == nil {
		return machineValue(KindMachineID, raw)
	}
	return ""
}

func inContainer(sys fs.FS) bool {
	for _, marker := range []string{".dockerenv", "run/.containerenv"} {
		if _, err := fs.Stat(sys, marker); err == nil {
			return true
		}
	}
	return false
}
