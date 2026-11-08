// Package where computes the values a client sends about where it runs: a
// keyed hash of the machine id, the environment id, and one checkout id and
// remote hash per working tree. Raw ids and keys never leave this package;
// callers get only the hashes.
package where

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Machine kinds, the prefix of a machine value.
const (
	kindDMI       = "dmi"
	kindMacOS     = "macos"
	kindWindows   = "windows"
	kindMachineID = "machine-id"
)

// machineKey is published on purpose: the hash keeps the raw id out of the
// server and its logs, and a machine value never grants anything.
const machineKey = "grelot machine"

// remoteKey is published for the same reason as machineKey: the hash keeps a
// private remote's URL out of the server, and a remote hash grants nothing.
const remoteKey = "grelot remote"

// Remote is the hash a checkout entry carries for its normalized remote,
// <host>/<owner path>/<name>, so the server compares remotes without seeing
// one.
func Remote(normalized string) string { return keyedHash([]byte(remoteKey), normalized) }

// placeholders are firmware ids shipped on many boards of one model, from
// https://rockstor.com/docs/howtos/about_appliance_id.html; only well-formed
// ones are listed, since the shape check refuses the rest.
var placeholders = map[string]bool{
	"03000200040005000006000700080009": true,
	"00020003000400050006000700080009": true,
	"5c4606fa192f453ab2997b088c63bb9b": true,
	"3139313835385a433135353130323750": true,
	"00000000000000000807060504030201": true,
}

// machineValue is "<kind>:<hash>" for a raw id that names one machine, or ""
// for one that does not (wrong shape, one repeated digit, a placeholder).
func machineValue(kind, raw string) string {
	norm := strings.ToLower(strings.TrimSpace(raw))
	digits := strings.ReplaceAll(norm, "-", "")
	if len(digits) != 32 || !isHex(digits) || placeholders[digits] ||
		strings.Count(digits, digits[:1]) == len(digits) {
		return ""
	}
	return kind + ":" + keyedHash([]byte(machineKey), norm)
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// keyedHash is the first 16 bytes of HMAC-SHA256(key, msg), in hex.
func keyedHash(key []byte, msg string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil)[:16])
}

// readSmall reads at most 256 bytes of name, enough for every id source.
func readSmall(fsys fs.FS, name string) (string, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 256))
	return string(b), err
}

// EnvironmentFile is the environment key's path under the user config dir.
func EnvironmentFile(configDir string) string {
	return filepath.Join(configDir, "grelot", "environment")
}

// Environment is the environment id: the keyed hash, under the key in file,
// of which side of the container check this process runs on. It creates the
// key when the file does not exist and answers "" when the file can neither
// be read nor created, or holds anything but one key.
func Environment(ctx context.Context, file string, container bool) string {
	key, ok := loadKey(ctx, file, true)
	if !ok {
		return ""
	}
	side := "host"
	if container {
		side = "container"
	}
	return keyedHash(key, side)
}

// CheckoutFile is the checkout key's path inside a working tree's git dir.
func CheckoutFile(gitDir string) string { return filepath.Join(gitDir, "grelot-checkout") }

// Checkout is the checkout id of the working tree at toplevel, whose own git
// dir is gitDir (each worktree of a clone has its own). It answers "" when
// the key can neither be read nor created.
func Checkout(ctx context.Context, gitDir, toplevel string) string {
	key, ok := loadKey(ctx, CheckoutFile(gitDir), false)
	if !ok {
		return ""
	}
	return checkoutID(key, toplevel, goos)
}

// goos is runtime.GOOS, a variable so the Windows fold runs on every
// platform's tests.
var goos = runtime.GOOS

func checkoutID(key []byte, toplevel, system string) string {
	return keyedHash(key, foldPath(toplevel, system))
}

// foldPath lowercases a Windows path and turns its backslashes to slashes:
// two tools can start git in one directory with another drive-letter case,
// git --show-toplevel repeats that case, and the id must not differ.
func foldPath(p, system string) string {
	if system != "windows" {
		return p
	}
	return strings.ToLower(strings.ReplaceAll(p, `\`, "/"))
}

// keyWait bounds how long a reader waits for a racing creator to write the
// key it has just created.
var keyWait = 500 * time.Millisecond

// loadKey reads the 16-byte key in file, creating it once when it does not
// exist. The create is exclusive and the file is never rewritten, because a
// new key would move every identity that used the old one.
func loadKey(ctx context.Context, file string, mkdir bool) ([]byte, bool) {
	if mkdir {
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			return nil, false
		}
	}
	if key, err := createKey(file); err == nil {
		return key, true
	} else if !errors.Is(err, fs.ErrExist) {
		return nil, false
	}
	deadline := time.Now().Add(keyWait)
	for {
		data, err := readSmall(os.DirFS(filepath.Dir(file)), filepath.Base(file))
		if err != nil {
			return nil, false
		}
		if data != "" || time.Now().After(deadline) {
			return parseKey(data)
		}
		// The file exists but is empty: another process created it and has
		// not written its key yet.
		t := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, false
		case <-t.C:
		}
	}
}

// createKey writes a new key to file, failing with fs.ErrExist when the file
// exists. The mode is set on the open file before the key is written, since
// the create mode is narrowed by the umask but can also be widened by an
// inherited ACL.
func createKey(file string) ([]byte, error) {
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	_ = f.Chmod(0o600) // Windows ignores it; the file and its key stay.
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	if _, err := f.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		return nil, err
	}
	return key, nil
}

func parseKey(data string) ([]byte, bool) {
	s := strings.TrimRight(data, "\r\n")
	if len(s) != 32 {
		return nil, false
	}
	key, err := hex.DecodeString(s)
	return key, err == nil
}
