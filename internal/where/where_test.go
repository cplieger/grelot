package where

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// testKey is the key file content the test vectors use: the 16 bytes
// 00 01 ... 0f in hex.
const testKey = "000102030405060708090a0b0c0d0e0f" // gitleaks:allow

func writeKey(t *testing.T, file, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMachineValue(t *testing.T) {
	// Expected values: printf '%s' <raw> | openssl dgst -sha256 -hmac 'grelot machine', cut to 32 hex.
	const dmi = "dmi:fc59f36df66249a0e8e6a447823ec7d1" // gitleaks:allow
	tests := []struct {
		name, kind, raw, want string
	}{
		{"dmi", kindDMI, "4c4c4544-0042-3510-8051-b4c04f334a32", dmi},
		{"upper case", kindDMI, "4C4C4544-0042-3510-8051-B4C04F334A32", dmi},
		{"trailing newline", kindDMI, "4c4c4544-0042-3510-8051-b4c04f334a32\n", dmi},
		{"all zero", kindDMI, "00000000-0000-0000-0000-000000000000", ""},
		{"all F", kindDMI, "FFFFFFFF-FFFF-FFFF-FFFF-FFFFFFFFFFFF", ""},
		{"placeholder", kindDMI, "03000200-0400-0500-0006-000700080009", ""},
		{"placeholder upper case", kindDMI, "5C4606FA-192F-453A-B299-7B088C63BB9B", ""},
		{"placeholder 3", kindDMI, "00020003-0004-0005-0006-000700080009", ""},
		{"placeholder 4", kindDMI, "31393138-3538-5a43-3135-353130323750", ""},
		{"placeholder 5", kindDMI, "00000000-0000-0000-0807-060504030201", ""},
		{"forty digits", kindDMI, "00000000-2093bfe3-e53b-4fc3-9cb1-9217ea6228c7", ""},
		{"non-hex character", kindDMI, "4c4c4544-0042-3510-8051-b4c04f334a3g", ""},
		{"empty", kindDMI, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := machineValue(tt.kind, tt.raw); got != tt.want {
				t.Errorf("machineValue(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestEnvironment_vectors(t *testing.T) {
	// printf '%s' host|container | openssl dgst -sha256 -mac HMAC -macopt hexkey:<testKey>, cut to 32 hex.
	file := EnvironmentFile(t.TempDir())
	writeKey(t, file, testKey+"\n")
	if got, want := Environment(t.Context(), file, false), "1807beded2f944a7cff070be623b7c8e"; got != want { // gitleaks:allow
		t.Errorf("host: got %q, want %q", got, want)
	}
	if got, want := Environment(t.Context(), file, true), "5a58d1c50e964c010381b73081640bfc"; got != want { // gitleaks:allow
		t.Errorf("container: got %q, want %q", got, want)
	}
	if got := readFile(t, file); got != testKey+"\n" {
		t.Errorf("file rewritten: %q", got)
	}
}

func TestEnvironment_createdOnceAndReused(t *testing.T) {
	file := EnvironmentFile(t.TempDir())
	first := Environment(t.Context(), file, false)
	if len(first) != 32 {
		t.Fatalf("first id = %q, want 32 hex characters", first)
	}
	content := readFile(t, file)
	if len(content) != 33 || content[32] != '\n' {
		t.Errorf("file content = %q, want 32 hex characters and a newline", content)
	}
	if again := Environment(t.Context(), file, false); again != first {
		t.Errorf("second read = %q, want %q", again, first)
	}
	if readFile(t, file) != content {
		t.Error("the second read rewrote the file")
	}
}

func TestEnvironment_concurrentCreatesShareOneKey(t *testing.T) {
	file := EnvironmentFile(t.TempDir())
	ids := make([]string, 8)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Go(func() { ids[i] = Environment(t.Context(), file, false) })
	}
	wg.Wait()
	for i, id := range ids {
		if id == "" || id != ids[0] {
			t.Errorf("ids[%d] = %q, want %q", i, id, ids[0])
		}
	}
}

func TestEnvironment_waitsForACreatorStillWriting(t *testing.T) {
	file := EnvironmentFile(t.TempDir())
	writeKey(t, file, "")
	done := make(chan string)
	go func() { done <- Environment(t.Context(), file, false) }()
	time.Sleep(50 * time.Millisecond) // the racing creator's write lands later
	if err := os.WriteFile(file, []byte(testKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := <-done, "1807beded2f944a7cff070be623b7c8e"; got != want { // gitleaks:allow
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEnvironment_unusableFile(t *testing.T) {
	t.Run("other content left untouched", func(t *testing.T) {
		file := EnvironmentFile(t.TempDir())
		writeKey(t, file, "not a key\n")
		if got := Environment(t.Context(), file, false); got != "" {
			t.Errorf("got %q, want empty", got)
		}
		if got := readFile(t, file); got != "not a key\n" {
			t.Errorf("file changed to %q", got)
		}
	})
	t.Run("short hex left untouched", func(t *testing.T) {
		file := EnvironmentFile(t.TempDir())
		writeKey(t, file, "0a0b0c\n")
		if got := Environment(t.Context(), file, false); got != "" {
			t.Errorf("got %q, want empty", got)
		}
		if got := readFile(t, file); got != "0a0b0c\n" {
			t.Errorf("file changed to %q", got)
		}
	})
	t.Run("empty file stays empty", func(t *testing.T) {
		old := keyWait
		keyWait = 20 * time.Millisecond
		t.Cleanup(func() { keyWait = old })
		file := EnvironmentFile(t.TempDir())
		writeKey(t, file, "")
		if got := Environment(t.Context(), file, false); got != "" {
			t.Errorf("got %q, want empty", got)
		}
		if got := readFile(t, file); got != "" {
			t.Errorf("file changed to %q", got)
		}
	})
	t.Run("config dir cannot be created", func(t *testing.T) {
		dir := t.TempDir()
		writeKey(t, filepath.Join(dir, "grelot"), "a file where the folder goes")
		if got := Environment(t.Context(), EnvironmentFile(dir), false); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestCheckout(t *testing.T) {
	// printf '%s' <toplevel> | openssl dgst -sha256 -mac HMAC -macopt hexkey:<testKey>, cut to 32 hex.
	gitDir := t.TempDir()
	writeKey(t, CheckoutFile(gitDir), testKey+"\n")
	if got, want := Checkout(t.Context(), gitDir, "/workspace/subflux"), "4bab12a948b11d81f6c089e7b0d1b2b3"; got != want { // gitleaks:allow
		t.Errorf("/workspace/subflux: got %q, want %q", got, want)
	}
	if got, want := Checkout(t.Context(), gitDir, "/tmp/subflux"), "7ec57acf62d88a6ef2ba8a4c3eb52631"; got != want { // gitleaks:allow
		t.Errorf("/tmp/subflux: got %q, want %q", got, want)
	}
}

func TestCheckout_createsItsKey(t *testing.T) {
	gitDir := t.TempDir()
	first := Checkout(t.Context(), gitDir, "/workspace/subflux")
	if len(first) != 32 {
		t.Fatalf("id = %q, want 32 hex characters", first)
	}
	if again := Checkout(t.Context(), gitDir, "/workspace/subflux"); again != first {
		t.Errorf("second id = %q, want %q", again, first)
	}
	other := Checkout(t.Context(), t.TempDir(), "/workspace/subflux")
	if other == first {
		t.Error("two git dirs with their own keys gave one id")
	}
}

func TestCheckout_copyAtAnotherPath(t *testing.T) {
	gitDir := t.TempDir()
	original := Checkout(t.Context(), gitDir, "/home/u/src/subflux")
	copied := t.TempDir()
	writeKey(t, CheckoutFile(copied), readFile(t, CheckoutFile(gitDir)))
	if got := Checkout(t.Context(), copied, "/home/u/backup/subflux"); got == original || got == "" {
		t.Errorf("copy at another path gave %q, original %q", got, original)
	}
}

func TestCheckoutID_windowsFold(t *testing.T) {
	key := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	const want = "c060a446e6cd60a1deb8692f59e9f647" // gitleaks:allow
	for _, p := range []string{`C:\Src\subflux`, "C:/src/subflux", "c:/src/subflux"} {
		if got := checkoutID(key, p, "windows"); got != want {
			t.Errorf("windows %q: got %q, want %q", p, got, want)
		}
	}
	if got := foldPath(`C:\Src\subflux`, "linux"); got != `C:\Src\subflux` {
		t.Errorf("linux fold changed the path to %q", got)
	}
}

func TestRemote(t *testing.T) {
	// printf '%s' <remote> | openssl dgst -sha256 -hmac 'grelot remote', cut to 32 hex.
	tests := []struct{ remote, want string }{
		{"github.com/cplieger/toolbelt", "3b9c6e0d9248a2015132854ae815633b"}, // gitleaks:allow
		{"gitlab.example.com/team/docs", "c5a54b6f1567ce7ff3e9f56d584b1498"}, // gitleaks:allow
	}
	for _, tt := range tests {
		if got := Remote(tt.remote); got != tt.want {
			t.Errorf("Remote(%q) = %q, want %q", tt.remote, got, tt.want)
		}
	}
}
