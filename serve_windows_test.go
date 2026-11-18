package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestRun_serverCommandsRefuseOnWindows(t *testing.T) {
	for _, cmd := range []string{"serve", "health", "setup-token", "recovery", "key"} {
		t.Run(cmd, func(t *testing.T) {
			var stderr bytes.Buffer
			if code := run([]string{cmd}, io.Discard, &stderr); code != 1 {
				t.Errorf("exit %d, want 1", code)
			}
			if got := stderr.String(); !strings.Contains(got, "runs on Linux and macOS") || strings.Count(got, "\n") != 1 {
				t.Errorf("stderr %q, want one line saying the server runs on Linux and macOS", got)
			}
		})
	}
	t.Run("no arguments", func(t *testing.T) {
		var stderr bytes.Buffer
		if code := run(nil, io.Discard, &stderr); code != 1 || !strings.Contains(stderr.String(), "grelot serve:") {
			t.Errorf("exit %d, stderr %q; want 1 refusing serve", code, stderr.String())
		}
	})
}
