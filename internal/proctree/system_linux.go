package proctree

import "os"

// System is the running system's process table.
func System() Table { return procFS{os.DirFS("/proc")} }
