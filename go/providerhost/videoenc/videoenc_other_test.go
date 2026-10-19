//go:build !windows

package videoenc

import (
	"errors"
	"path/filepath"
	"testing"
)

// INF-VE1: off Windows the encoder answers unsupported and writes nothing.
func TestUnsupportedOffWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.mp4")
	o := Options{Width: 256, Height: 144, FPS: 16, Layout: RGB24}
	if _, err := Create(path, o); !errors.Is(err, ErrNoSystemEncoder) {
		t.Fatalf("Create: %v", err)
	}
	if _, _, err := Encode(t.TempDir(), o, [][]byte{frame(0, 256, 144, RGB24)}); !errors.Is(err, ErrNoSystemEncoder) {
		t.Fatalf("Encode: %v", err)
	}
}
