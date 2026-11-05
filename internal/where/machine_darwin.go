package where

import (
	"context"
	"os/exec"
	"regexp"
	"time"
)

var ioregUUID = regexp.MustCompile(`"IOPlatformUUID" = "([^"]+)"`)

// Machine is this machine's value for where.machine, or "" when no source
// names one. IOKit needs cgo and the release binaries are static, so the
// value comes from ioreg.
func Machine(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
	if err != nil {
		return ""
	}
	m := ioregUUID.FindSubmatch(out)
	if m == nil {
		return ""
	}
	return machineValue(KindMacOS, string(m[1]))
}

// InContainer is false: the container check reads Linux marker files.
func InContainer() bool { return false }
