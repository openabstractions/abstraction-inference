//go:build windows

package enginelaunch

import (
	"errors"
	"net"
	"os"
	"time"
)

// A packaged parent's virtualized LocalAppData can let AF_UNIX bind while
// every connect returns WSAEINVAL. Prove both operations on the exact
// launch path before starting an engine, including for an explicit root.
var probeSocketTransport = func(socket string) error {
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	conn, dialErr := net.DialTimeout("unix", socket, time.Second)
	if conn != nil {
		_ = conn.Close()
	}
	closeErr := listener.Close()
	removeErr := os.Remove(socket)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(dialErr, closeErr, removeErr)
}
