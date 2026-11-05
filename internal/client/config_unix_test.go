//go:build unix

package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig_modeWithToken(t *testing.T) {
	t.Run("readable by others refused", func(t *testing.T) {
		dir := writeConfig(t, "server: https://g.example\ntoken: test-token\n", 0o644)
		_, err := LoadConfig(dir, env(), noHostname)
		if err == nil || !strings.Contains(err.Error(), "chmod 600 "+ConfigFile(dir)) {
			t.Errorf("err = %v, want the chmod 600 fix", err)
		}
	})
	t.Run("readable by the group refused", func(t *testing.T) {
		dir := writeConfig(t, "server: https://g.example\ntoken: test-token\n", 0o640)
		if _, err := LoadConfig(dir, env(), noHostname); err == nil {
			t.Error("LoadConfig of a 0640 file holding the token succeeded")
		}
	})
	t.Run("0600 accepted", func(t *testing.T) {
		dir := writeConfig(t, "server: https://g.example\ntoken: test-token\n", 0o600)
		if _, err := LoadConfig(dir, env(), noHostname); err != nil {
			t.Errorf("LoadConfig: %v", err)
		}
	})
	t.Run("no token in the file accepted", func(t *testing.T) {
		tokenFile := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(tokenFile, []byte("file-token"), 0o600); err != nil {
			t.Fatal(err)
		}
		dir := writeConfig(t, "server: https://g.example\n", 0o644)
		if _, err := LoadConfig(dir, env(EnvTokenFile, tokenFile), noHostname); err != nil {
			t.Errorf("LoadConfig: %v", err)
		}
	})
}
