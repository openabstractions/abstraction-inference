//go:build !windows

package enginehttp

import "syscall"

// notListeningErrnos are the connect errors of an engine that is still
// starting: the connection refused, the socket file not made yet, a full
// listen queue, and a timed out, in-progress or interrupted connect.
var notListeningErrnos = []syscall.Errno{syscall.ECONNREFUSED, syscall.ENOENT, syscall.EAGAIN,
	syscall.ETIMEDOUT, syscall.EINPROGRESS, syscall.EINTR}
