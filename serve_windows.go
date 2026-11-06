package main

import (
	"fmt"
	"io"
)

// serverCommand refuses serve and health on Windows, before any store is
// opened: the store's database library does not run there.
func serverCommand(cmd string, stderr io.Writer) int {
	fmt.Fprintf(stderr, "grelot %s: the grelot server runs on Linux and macOS; on Windows grelot is the client only\n", cmd)
	return 1
}
