package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cplieger/grelot/internal/provider"
)

// providerCommand runs grelot provider check <file>: exit 0 when the file is
// valid, 1 with one line per problem on stderr when it is not or cannot be
// read, 2 on a usage error.
func providerCommand(args []string, stderr io.Writer) int {
	if len(args) != 2 || args[0] != "check" {
		fmt.Fprintln(stderr, "grelot provider: usage: grelot provider check <file>")
		return 2
	}
	file := args[1]
	name, ok := strings.CutSuffix(filepath.Base(file), ".yaml")
	if !ok {
		fmt.Fprintf(stderr, "grelot provider check: %s: a provider file is named <name>.yaml\n", file)
		return 1
	}
	data, err := os.ReadFile(file) //nolint:gosec // the user names the file to check on the command line
	if err != nil {
		fmt.Fprintf(stderr, "grelot provider check: %v\n", err)
		return 1
	}
	if err := provider.Check(name, data); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
