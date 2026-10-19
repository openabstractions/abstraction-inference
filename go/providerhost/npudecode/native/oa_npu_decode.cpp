// oa_npu_decode: OA's flat C API over XRT for the NPU decoder of
// exp-fixed3-v1 units, loaded from Go with LoadLibraryEx and called through
// syscall (no cgo). XRT's C API opens devices and buffers but cannot create
// the hw_context an NPU kernel runs in, and its C++ API passes std::string
// and std::map, which only an MSVC C++ caller can pass: hence this shim.
//
// The API is four calls. oa_npu_query names device 0. oa_npu_open loads one
// prebuilt design (xclbin and instruction stream bytes), creates `sets` pairs of
// host buffers of in_bytes and out_bytes and returns their mapped
// addresses. oa_npu_decode runs the design once on one pair, waiting at most
// timeout_ms. oa_npu_close frees everything.
//
// Every call that can fail returns 0 on success, -1 on an error and -2 on a
// timeout, and writes the reason into the caller's err buffer (no
// thread-local state: a Go caller may change OS threads between calls).
// After an error or a timeout the handle refuses every further decode.
//
// Runtime dependency: xrt_coreutil.dll, which AMD's NPU driver installs in
// System32 (XRT, Apache-2.0 user-space code), and the MSVC runtime it
// already needs. Build: build_shim.bat.

#include <chrono>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <exception>
#include <stdexcept>
#include <string>
#include <vector>

#include "xrt/xrt_bo.h"
#include "xrt/xrt_device.h"
#include "xrt/xrt_hw_context.h"
#include "xrt/xrt_kernel.h"

#define API extern "C" __declspec(dllexport)

namespace {

const int kError = -1;
const int kTimeout = -2;

struct npu {
  xrt::device device;
  xrt::hw_context context;
  xrt::kernel kernel;
  xrt::bo instr;
  uint32_t ninstr = 0;
  std::vector<xrt::bo> in, out;
  std::vector<xrt::run> runs;
  bool broken = false;
};

int fail(char *err, uint32_t cap, const char *what, const char *why, int code = kError) {
  if (err && cap) {
    std::snprintf(err, cap, "%s: %s", what, why);
  }
  return code;
}

}  // namespace

// oa_npu_query writes device 0's name into info. It fails when XRT finds no
// device.
API int oa_npu_query(char *info, uint32_t info_cap, char *err, uint32_t err_cap) {
  try {
    xrt::device d(0u);
    std::string name = d.get_info<xrt::info::device::name>();
    if (info && info_cap) {
      std::snprintf(info, info_cap, "%s", name.c_str());
    }
    return 0;
  } catch (const std::exception &e) {
    return fail(err, err_cap, "query", e.what());
  } catch (...) {
    return fail(err, err_cap, "query", "unknown exception");
  }
}

// oa_npu_open opens device 0, registers the xclbin (xclbin_len bytes),
// creates a hardware context, the kernel, the instruction buffer (insts:
// insts_len bytes of 32-bit words) and `sets` input and output buffers, and
// writes the buffers' mapped addresses into in_ptrs[sets] and
// out_ptrs[sets]. The caller reads and checks both files; no path crosses
// this boundary.
API int oa_npu_open(const char *xclbin, uint64_t xclbin_len, const char *insts, uint64_t insts_len,
                    const char *kernel_name, uint64_t in_bytes, uint64_t out_bytes, uint32_t sets, void **handle,
                    void **in_ptrs, void **out_ptrs, char *err, uint32_t err_cap) {
  npu *n = nullptr;
  try {
    if (sets == 0 || sets > 4 || in_bytes == 0 || out_bytes == 0) {
      throw std::invalid_argument("sets must be 1 to 4 and both buffer sizes positive");
    }
    if (xclbin_len == 0 || insts_len == 0 || insts_len % 4) {
      throw std::invalid_argument("xclbin empty, or instructions empty or not whole 32-bit words");
    }
    n = new npu();
    n->device = xrt::device(0u);
    xrt::xclbin xb{std::vector<char>(xclbin, xclbin + xclbin_len)};
    n->device.register_xclbin(xb);
    n->context = xrt::hw_context(n->device, xb.get_uuid());
    n->kernel = xrt::kernel(n->context, std::string{kernel_name});
    n->ninstr = static_cast<uint32_t>(insts_len / 4);
    n->instr = xrt::bo(n->device, insts_len, xrt::bo::flags::cacheable, n->kernel.group_id(1));
    std::memcpy(n->instr.map<void *>(), insts, insts_len);
    n->instr.sync(XCL_BO_SYNC_BO_TO_DEVICE);
    for (uint32_t s = 0; s < sets; ++s) {
      n->in.emplace_back(n->device, in_bytes, xrt::bo::flags::host_only, n->kernel.group_id(3));
      n->out.emplace_back(n->device, out_bytes, xrt::bo::flags::host_only, n->kernel.group_id(4));
      xrt::run r(n->kernel);
      r.set_arg(0, 3u);  // opcode 3: run the instruction stream
      r.set_arg(1, n->instr);
      r.set_arg(2, n->ninstr);
      r.set_arg(3, n->in.back());
      r.set_arg(4, n->out.back());
      n->runs.push_back(r);
      in_ptrs[s] = n->in.back().map<void *>();
      out_ptrs[s] = n->out.back().map<void *>();
    }
    *handle = n;
    return 0;
  } catch (const std::exception &e) {
    delete n;
    return fail(err, err_cap, "open", e.what());
  } catch (...) {
    delete n;
    return fail(err, err_cap, "open", "unknown exception");
  }
}

// oa_npu_decode syncs input buffer `set` to the device, runs the design
// once, waits at most timeout_ms, and syncs output buffer `set` back. *ns
// receives the time from the run's start to its completion.
API int oa_npu_decode(void *handle, uint32_t set, uint32_t timeout_ms, uint64_t *ns, char *err, uint32_t err_cap) {
  auto *n = static_cast<npu *>(handle);
  if (n->broken) {
    return fail(err, err_cap, "decode", "an earlier run failed; the device is not used again");
  }
  if (set >= n->runs.size() || timeout_ms == 0) {
    return fail(err, err_cap, "decode", "no such buffer set, or no time limit");
  }
  try {
    n->in[set].sync(XCL_BO_SYNC_BO_TO_DEVICE);
    auto t0 = std::chrono::steady_clock::now();
    n->runs[set].start();
    ert_cmd_state state = n->runs[set].wait(std::chrono::milliseconds{timeout_ms});
    *ns = static_cast<uint64_t>(
        std::chrono::duration_cast<std::chrono::nanoseconds>(std::chrono::steady_clock::now() - t0).count());
    if (state == ERT_CMD_STATE_TIMEOUT) {
      n->broken = true;
      ert_cmd_state aborted = ERT_CMD_STATE_MAX;
      try {
        aborted = n->runs[set].abort();
      } catch (...) {
      }
      char why[96];
      std::snprintf(why, sizeof why, "no completion within %u ms; abort left state %d", timeout_ms,
                    static_cast<int>(aborted));
      return fail(err, err_cap, "decode", why, kTimeout);
    }
    if (state != ERT_CMD_STATE_COMPLETED) {
      n->broken = true;
      return fail(err, err_cap, "decode", ("run ended in state " + std::to_string(static_cast<int>(state))).c_str());
    }
    n->out[set].sync(XCL_BO_SYNC_BO_FROM_DEVICE);
    return 0;
  } catch (const std::exception &e) {
    n->broken = true;
    return fail(err, err_cap, "decode", e.what());
  } catch (...) {
    n->broken = true;
    return fail(err, err_cap, "decode", "unknown exception");
  }
}

// oa_npu_close frees the buffers, the kernel, the context and the device.
// No exception leaves it: XRT's destructors run inside a C caller.
API void oa_npu_close(void *handle) {
  try {
    delete static_cast<npu *>(handle);
  } catch (...) {
  }
}
