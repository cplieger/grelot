//go:build !linux && !darwin && !windows

package admin

import (
	"errors"
	"net"
)

// peerUID refuses every peer on a system whose peer credentials this
// package does not read.
func peerUID(*net.UnixConn) (int, error) {
	return -1, errors.New("peer credentials are read on Linux and macOS only")
}
