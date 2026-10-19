//go:build !windows

package enginelaunch

// The profile-redirection failure is specific to Windows packaged parents.
var probeSocketTransport = func(string) error { return nil }
