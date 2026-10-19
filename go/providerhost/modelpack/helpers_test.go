package modelpack

import (
	"encoding/binary"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// f16FromBF16 rounds f to BF16 and then to F16, the way GGUF files converted
// from BF16 checkpoints hold their F16 tensors.
func f16FromBF16(f float32) uint16 {
	return f16(math.Float32frombits(uint32(bf16(f)) << 16))
}

// f16ToF32 widens an IEEE binary16 value.
func f16ToF32(h uint16) float32 {
	sign := uint32(h&0x8000) << 16
	exp := uint32(h>>10) & 0x1f
	mant := uint32(h & 0x3ff)
	switch {
	case exp == 0x1f:
		return math.Float32frombits(sign | 0x7f800000 | mant<<13)
	case exp == 0 && mant == 0:
		return math.Float32frombits(sign)
	case exp == 0:
		e := uint32(113)
		for mant&0x400 == 0 {
			mant <<= 1
			e--
		}
		return math.Float32frombits(sign | e<<23 | (mant&0x3ff)<<13)
	}
	return math.Float32frombits(sign | (exp+112)<<23 | mant<<13)
}

func randomBytes(n int, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, 7))
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(r.Uint32())
	}
	return out
}

// bsdtar is a libarchive tar, which reads zip archives: Windows' own
// System32\tar.exe there, and elsewhere a tar on PATH whose --version names
// bsdtar. GNU tar, which reads no zip, is skipped.
func bsdtar() (string, bool) {
	if runtime.GOOS == "windows" {
		p := filepath.Join(os.Getenv("SystemRoot"), "System32", "tar.exe")
		_, err := os.Stat(p)
		return p, err == nil && os.Getenv("SystemRoot") != ""
	}
	p, err := exec.LookPath("tar")
	if err != nil {
		return "", false
	}
	out, err := exec.Command(p, "--version").Output()
	return p, err == nil && strings.Contains(string(out), "bsdtar")
}

func values16(vs ...uint16) []byte {
	out := make([]byte, 2*len(vs))
	for i, v := range vs {
		binary.LittleEndian.PutUint16(out[2*i:], v)
	}
	return out
}
