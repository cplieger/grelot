package main

import (
	"fmt"
	"io"
)

// serverCommand refuses every server command on Windows, before any store
// is opened: the store's database library does not run there.
func serverCommand(cmd string, _ []string, _, stderr io.Writer) int {
	fmt.Fprintf(stderr, "grelot %s: the grelot server runs on Linux and macOS; on Windows grelot is the client only\n", cmd)
	return 1
}
