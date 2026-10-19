//go:build !windows

package enginelaunch

import "errors"

var errNoWindows = errors.New("the Windows kernel, token and registry exist on Windows only")

func kernelWindowsDir() (string, error) { return "", errNoWindows }

// SystemRegistry reports that the registry exists on Windows only.
func SystemRegistry(hive, path, name string) (string, error) { return "", errNoWindows }

// facts outside Windows computes nothing: a provider that names its own
// Windows folder there gets every name left out with a diagnostic.
type facts struct{}

func newFacts(string, RegistryValue) *facts { return &facts{} }

func (*facts) value(string) (string, error) { return "", errNoWindows }
