package statefile

import (
	"os"
	"path/filepath"
)

// Replace writes data to file through a temporary file beside it, which
// os.CreateTemp makes mode 0600, then renames it over file. It creates the
// directory when it is missing. filesafe is Unix-only because Windows does
// not promise an atomic rename over an existing file
// (https://go.dev/issue/22397).
func Replace(file string, data []byte) error {
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(file)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(data)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	return os.Rename(tmp.Name(), file)
}
