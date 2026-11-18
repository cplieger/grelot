package webui

import (
	"embed"
	"io/fs"
)

// static holds the bundle cmd/bundle writes; on a clone that has not run
// it, only static/.gitkeep, which keeps the embed compiling.
//
//go:embed all:static
var static embed.FS

// Files is the built UI, rooted at static/.
func Files() fs.FS {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		panic(err) // "static" is a valid path, so Sub cannot fail
	}
	return sub
}
