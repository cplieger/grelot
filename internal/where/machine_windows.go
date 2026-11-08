package where

import (
	"context"

	"golang.org/x/sys/windows/registry"
)

// Machine is this machine's value for where.machine, or "" when no source
// names one. The 64-bit view is read so a 32-bit build gets the same value.
func Machine(context.Context) string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`,
		registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return ""
	}
	defer k.Close()
	raw, _, err := k.GetStringValue("MachineGuid")
	if err != nil {
		return ""
	}
	return machineValue(kindWindows, raw)
}

// InContainer is false: the container check reads Linux marker files.
func InContainer() bool { return false }
