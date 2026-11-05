package transport

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

const fakeDoorArg = "fake-door"

// TestMain lets the test binary stand in for an exec door's program: run as
// <binary> fake-door <ctl> <record> <args...>, it appends args to record as
// one JSON line and then acts as ctl's content says: ok, fail or sleep.
func TestMain(m *testing.M) {
	if len(os.Args) > 3 && os.Args[1] == fakeDoorArg {
		os.Exit(fakeDoor(os.Args[2], os.Args[3], os.Args[4:]))
	}
	os.Exit(m.Run())
}

func fakeDoor(ctl, record string, args []string) int {
	mode, err := os.ReadFile(ctl)
	if err != nil {
		return 9
	}
	line, _ := json.Marshal(args)
	f, err := os.OpenFile(record, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 9
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
	fmt.Println("child stdout")
	fmt.Fprintln(os.Stderr, "child stderr")
	switch strings.TrimSpace(string(mode)) {
	case "ok":
		return 0
	case "sleep":
		time.Sleep(30 * time.Second)
		return 0
	default:
		return 3
	}
}
