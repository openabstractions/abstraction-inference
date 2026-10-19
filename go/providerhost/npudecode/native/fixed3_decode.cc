// fixed3_decode: one exp-fixed3-v1 chunk (inference CONTRACT.md INF-MP8) of
// 4,096 BF16 values on one AIE2P core of an XDNA 2 NPU.
//
// Chunk layout (5,696 bytes):
//   [0, 64)      header; byte 0 is the window base b, at most 248
//   [64, 576)    plane 0: bit 0 of each 3-bit exponent code
//   [576, 1088)  plane 1: bit 1
//   [1088, 1600) plane 2: bit 2
//   [1600, 5696) sign and mantissa bytes, value order: sign << 7 | mantissa
// Value v = 512 * blk + 64 * i + j has its code bits in bit i of byte
// 64 * blk + j of each plane. The exponent is b + code for every code, so an
// escaped value (code 7) leaves with exponent b + 7, as OA's fused CPU
// decode leaves it; the fill's hash pass applies the escape (INF-MP7).
// Output: 4,096 little-endian BF16 values (8,192 bytes).
//
// Built by build_kernel.ps1 with the IRON toolchain (aie_api headers and the
// Peano compiler); measured in research/model-locators/
// GPU-AND-MATRIX-DECOMPRESSION.md section 13 as tbe_decode.cc.

#define NOCPP

#include <stdint.h>

#include <aie_api/aie.hpp>

extern "C" {
void fixed3_decode(uint8_t *restrict in, uint8_t *restrict out) {
  const uint8_t base = in[0];
  const uint8_t *restrict p0 = in + 64;
  const uint8_t *restrict p1 = in + 576;
  const uint8_t *restrict p2 = in + 1088;
  const uint8_t *restrict sm = in + 1600;
  for (int blk = 0; blk < 8; ++blk) {
    ::aie::vector<uint8, 64> v0 = ::aie::load_v<64>(p0 + blk * 64);
    ::aie::vector<uint8, 64> v1 = ::aie::load_v<64>(p1 + blk * 64);
    ::aie::vector<uint8, 64> v2 = ::aie::load_v<64>(p2 + blk * 64);
#pragma clang loop min_iteration_count(8)
    for (int i = 0; i < 8; ++i) {
      ::aie::vector<uint8, 64> b0 = ::aie::bit_and((uint8)1, ::aie::downshift(v0, i));
      ::aie::vector<uint8, 64> b1 = ::aie::bit_and((uint8)2, ::aie::upshift(::aie::downshift(v1, i), 1));
      ::aie::vector<uint8, 64> b2 = ::aie::bit_and((uint8)4, ::aie::upshift(::aie::downshift(v2, i), 2));
      ::aie::vector<uint8, 64> e = ::aie::add(::aie::bit_or(b0, ::aie::bit_or(b1, b2)), base);
      ::aie::vector<uint8, 64> s = ::aie::load_v<64>(sm + blk * 512 + i * 64);
      ::aie::vector<uint8, 64> lo =
          ::aie::bit_or(::aie::upshift(::aie::bit_and((uint8)1, e), 7), ::aie::bit_and((uint8)0x7F, s));
      ::aie::vector<uint8, 64> hi = ::aie::bit_or(::aie::bit_and((uint8)0x80, s), ::aie::downshift(e, 1));
      auto [z0, z1] = ::aie::interleave_zip(lo, hi, 1);
      uint8_t *o = out + (blk * 512 + i * 64) * 2;
      ::aie::store_v(o, z0);
      ::aie::store_v(o + 64, z1);
    }
  }
}
}
