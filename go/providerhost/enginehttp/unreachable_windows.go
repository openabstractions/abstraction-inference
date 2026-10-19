//go:build windows

package enginehttp

import "syscall"

// notListeningErrnos are the Winsock connect errors of an engine that is
// still starting: WSAECONNREFUSED (10061), which an AF_UNIX path with no
// listener or no socket file yet also reads (measured 2026-10-03), and
// WSAETIMEDOUT, WSAEWOULDBLOCK, WSAEINPROGRESS and WSAEINTR. A launch
// folder that is gone reads WSAENETDOWN (10050) and a path in a packaged
// app's redirected folder WSAEINVAL (10022); both repeat.
var notListeningErrnos = []syscall.Errno{10061, 10060, 10035, 10036, 10004}
