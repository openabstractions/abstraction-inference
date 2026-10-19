package modelpack

import (
	"encoding/binary"
	"math"
	"math/rand/v2"
)

// bf16 rounds f to BF16, nearest even, the way PyTorch's conversion does.
func bf16(f float32) uint16 {
	b := math.Float32bits(f)
	if f != f {
		return uint16(b>>16) | 0x40
	}
	return uint16((b + 0x7fff + (b>>16)&1) >> 16)
}

// f16 rounds f to IEEE binary16, nearest even, with subnormals.
func f16(f float32) uint16 {
	b := math.Float32bits(f)
	sign := uint16(b>>16) & 0x8000
	exp := int(b>>23) & 0xff
	mant := b & 0x7fffff
	if exp == 0xff {
		if mant != 0 {
			return sign | 0x7e00
		}
		return sign | 0x7c00
	}
	e := exp - 112
	if e >= 31 {
		return sign | 0x7c00
	}
	if e <= 0 {
		if e < -10 {
			return sign
		}
		full := mant | 0x800000
		shift := uint(14 - e)
		m := full >> shift
		rem, half := full&(1<<shift-1), uint32(1)<<(shift-1)
		if rem > half || rem == half && m&1 == 1 {
			m++
		}
		return sign | uint16(m)
	}
	out := uint32(e)<<10 | mant>>13
	rem := mant & 0x1fff
	if rem > 0x1000 || rem == 0x1000 && out&1 == 1 {
		out++
	}
	return sign | uint16(out)
}

// weights draws n values of a trained layer's typical shape: normal with a
// standard deviation of 0.02, and one value in 64 from a wider normal for
// outliers. It returns their little-endian 16-bit form. MeasureRates decodes
// such values; GPU-AND-MATRIX-DECOMPRESSION.md §11.1 measured the same shape.
func weights(n int, seed uint64, convert func(float32) uint16) []byte {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	out := make([]byte, 2*n)
	for i := range n {
		sd := 0.02
		if r.IntN(64) == 0 {
			sd = 0.3
		}
		binary.LittleEndian.PutUint16(out[2*i:], convert(float32(r.NormFloat64()*sd)))
	}
	return out
}
