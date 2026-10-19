package enginelaunch

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	identity "github.com/openabstractions/abstraction-identity"

	"github.com/openabstractions/abstraction-identity/go/privatedir"
)

// SocketName is the engine's socket file inside its launch directory. The
// ".sock" ending is what llama-server b10825 reads as an AF_UNIX address in
// --host (tools/server/server-http.cpp:438-444).
const SocketName = "e.sock"

// MaxSocketPath is the longest socket path the kit hands an engine, in
// bytes. sockaddr_un.sun_path holds 108 bytes on Windows, Linux and macOS
// (afunix.h UNIX_PATH_MAX); Go's dialer keeps one for the terminating zero.
const MaxSocketPath = 107

// launchDirBytes is the per-launch directory name: 8 hex digits, short
// enough that a socket under a per-user state folder fits MaxSocketPath.
const launchDirBytes = 4

// ErrSocketPath reports a socket directory the kit refuses to make: a parent
// that is not absolute, a socket path longer than MaxSocketPath or outside
// printable ASCII, or a launch directory that already exists.
var ErrSocketPath = errors.New("enginelaunch: engine socket path refused")

// ErrSocketTransport reports a launch path on which this process cannot
// complete a local AF_UNIX connection, even when it can bind a listener.
var ErrSocketTransport = errors.New("enginelaunch: engine socket transport unreachable")

// ErrNetworkSocket reports an engine whose contained process tree holds a
// TCP listener or a bound UDP socket, or whose sockets could not be read
// (INF-EL2).
var ErrNetworkSocket = errors.New("enginelaunch: engine holds a network socket")

// A SocketDir is one engine launch's private directory and the socket path
// in it the engine is told to listen on (INF-EL2). Only this account and
// SYSTEM can open the directory on Windows; elsewhere its mode is 0700.
type SocketDir struct {
	// Dir is the launch directory, created by NewSocketDir.
	Dir string
	// Socket is Dir's SocketName, at most MaxSocketPath bytes.
	Socket string
	// lock holds Dir's held file from creation to Remove, so a sweep by
	// another provider sharing the root never takes a directory whose
	// engine is still starting.
	lock *dirLock
}

// heldName is the file in each launch directory its provider holds locked
// while the directory is in use.
const heldName = "held"

// SocketPathFits reports whether a launch directory under parent gives a
// socket path the engine can bind, so a provider can refuse a state folder
// at configuration instead of at the first load.
func SocketPathFits(parent string) error {
	_, err := socketPath(parent, strings.Repeat("0", 2*launchDirBytes))
	return err
}

func socketPath(parent, name string) (string, error) {
	if !filepath.IsAbs(parent) {
		return "", fmt.Errorf("%w: the socket parent %q is not absolute", ErrSocketPath, parent)
	}
	socket := filepath.Join(filepath.Clean(parent), name, SocketName)
	if len(socket) > MaxSocketPath {
		return "", fmt.Errorf("%w: %s is %d bytes, longer than the %d an AF_UNIX address holds; choose a shorter state folder", ErrSocketPath, socket, len(socket), MaxSocketPath)
	}
	for _, r := range socket {
		// llama-server reads its command line in the ANSI code page on
		// Windows, so a path outside printable ASCII would reach bind(2)
		// changed.
		if r < 0x20 || r > 0x7e {
			return "", fmt.Errorf("%w: %q holds a character outside printable ASCII", ErrSocketPath, socket)
		}
	}
	return socket, nil
}

// NewSocketDir creates a fresh launch directory under parent, the socket
// root (SocketRoot, or a provider's own folder). It creates the root private
// to this account if it is absent and refuses one that another principal
// owns, that is open beyond this account (and SYSTEM on Windows), or that is
// a link (ErrSocketRoot). The launch directory's name is random and its
// creation refuses an existing path, so no earlier file, socket or link can
// stand where the engine binds.
func NewSocketDir(parent string) (*SocketDir, error) {
	var raw [launchDirBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, fmt.Errorf("enginelaunch: launch directory name: %w", err)
	}
	socket, err := socketPath(parent, hex.EncodeToString(raw[:]))
	if err != nil {
		return nil, err
	}
	if err := ensurePrivateRoot(parent); err != nil {
		return nil, err
	}
	dir := filepath.Dir(socket)
	if err := privatedir.Make(dir); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("%w: %s already exists", ErrSocketPath, dir)
		}
		return nil, fmt.Errorf("enginelaunch: launch directory: %w", err)
	}
	lock, err := lockDir(dir)
	if err != nil {
		//unchecked: the lock failure is the error returned; the directory is empty
		os.RemoveAll(dir)
		return nil, fmt.Errorf("enginelaunch: launch directory lock: %w", err)
	}
	endpoint := &SocketDir{Dir: dir, Socket: socket, lock: lock}
	if err := probeSocketTransport(socket); err != nil {
		return nil, errors.Join(fmt.Errorf("%w: %s: %w", ErrSocketTransport, socket, err), endpoint.Remove())
	}
	return endpoint, nil
}

// Remove deletes the launch directory and the socket the engine left in it.
// Call it once the engine's exit is confirmed.
func (d *SocketDir) Remove() error {
	if d == nil || d.Dir == "" {
		return nil
	}
	if err := d.lock.release(); err != nil {
		return err
	}
	return os.RemoveAll(d.Dir)
}

// SweepSocketDirs removes the launch directories a provider that stopped
// without cleaning up left under parent: each that no provider holds and
// whose socket answers no dial, or that holds no socket. A directory a
// provider holds, its engine starting or running, stays, and so does one
// whose socket still answers. It returns one error per directory it could
// not remove; a missing parent is no error.
func SweepSocketDirs(parent string) []error {
	entries, err := os.ReadDir(parent)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []error{err}
	}
	var errs []error
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || len(name) != 2*launchDirBytes {
			continue
		}
		if _, err := hex.DecodeString(name); err != nil {
			continue
		}
		dir := filepath.Join(parent, name)
		if held(dir) {
			continue
		}
		if answers(filepath.Join(dir, SocketName)) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// answers reports whether something accepts a dial on socket. The dial
// sends no byte.
func answers(socket string) bool {
	if _, err := os.Lstat(socket); err != nil {
		return false
	}
	c, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		return false
	}
	//unchecked: the dial only asked whether a listener exists; nothing was sent
	c.Close()
	return true
}

// CheckEngineSockets is INF-EL2 for an engine's transport, the one check a
// provider calls after readiness and while the engine runs, whichever way
// the engine serves. An engine on a socket path (loopbackPort 0) owns no
// network socket at all, read from all, every TCP socket in any state and
// every UDP socket (CheckSocketOnly). An engine that serves TCP only holds
// its listener on 127.0.0.1 at loopbackPort and no listener beyond
// loopback, read from listeners (CheckLoopback). A nil reader reads
// identity's tables.
func CheckEngineSockets(listeners, all func() ([]identity.Socket, error), contains identity.TreeContains, loopbackPort int) error {
	if loopbackPort == 0 {
		return CheckSocketOnly(all, contains)
	}
	return CheckLoopback(listeners, loopbackPort, contains)
}

// CheckSocketOnly applies INF-EL2 to an engine that serves on a socket path:
// the engine's contained process tree holds no TCP socket in any state,
// listening, connecting or established, outbound included, and no bound UDP
// socket, on any address. Membership is proved by process id and creation
// time through contains, so a reused process id is never counted. sockets
// reads the owner tables; nil reads them through identity.AllSockets, and a
// platform without them refuses. Every refusal wraps ErrNetworkSocket; the
// caller stops the tree. The socket's server is checked on every connection
// by the client (providerhost/enginehttp, identity ID-T8).
func CheckSocketOnly(sockets func() ([]identity.Socket, error), contains identity.TreeContains) error {
	if contains == nil {
		return fmt.Errorf("%w: no process tree to check", ErrNetworkSocket)
	}
	if sockets == nil {
		sockets = identity.AllSockets
	}
	owned, err := sockets()
	if err != nil {
		return fmt.Errorf("%w: owner tables: %v", ErrNetworkSocket, err)
	}
	member := identity.OwnedByTree(contains)
	seen := map[int]bool{}
	for _, s := range owned {
		in, asked := seen[s.PID]
		if !asked {
			in = s.PID > 0 && member(s.PID)
			seen[s.PID] = in
		}
		if in {
			return fmt.Errorf("%w: %s %s:%d owned by process %d", ErrNetworkSocket, s.Proto, s.Addr, s.Port, s.PID)
		}
	}
	return nil
}
