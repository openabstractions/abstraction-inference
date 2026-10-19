# npudecode native sources

*For maintainers building the NPU decoder.*

OA's sources of the NPU decoder that package `npudecode` loads. They sit
beside the Go package because they are its other half, and in a folder of
their own because Go refuses C++ files in a package directory without cgo.
Nothing here is third-party code; the build uses third-party tools and
headers, listed below. Built files go to a kit folder outside the tree.

| File | What |
| --- | --- |
| `oa_npu_decode.cpp` | the shim: four C calls (query, open, decode, close) over XRT's C++ API |
| `build_shim.bat` | builds `oa_npu_decode.dll` with MSVC |
| `fixed3_decode.cc` | the AIE2P kernel: one chunk of 4,096 BF16 values per call |
| `fixed3_design.py` | the IRON design: 64 units per batch, 8 shim streams and tiles, slot stride 397,376 bytes |
| `build_kernel.ps1` | compiles the design into `fixed3.xclbin`, `fixed3.insts` and `fixed3.json` |
| `make_kit.ps1` | runs both builds in a scratch folder and writes the kit: the four files and `kit.json`, their SHA-256, in a folder private to this account |

## Dependencies

At run time the shim needs only what an AMD NPU driver installs:
`C:\Windows\System32\xrt_coreutil.dll` (XRT user-mode library, AMD's build
of XRT, Apache-2.0 user-space code; here version 32.0.20102.3931, signed by
Microsoft Windows Hardware Compatibility Publisher) and the MSVC runtime
(`msvcp140.dll`, `vcruntime140.dll`), which that DLL itself needs. The
shim links the runtime dynamically (`/MD`) because XRT's C++ API passes
`std::string` across the DLL boundary. Go loads the shim by its absolute
path with `LoadLibraryEx` and `LOAD_LIBRARY_SEARCH_SYSTEM32`, so its imports
come from System32 alone.

At build time:

- `build_shim.bat`: Visual Studio 18 (MSVC) and the Windows XRT SDK 2.21.75
  headers and import library `xrt_coreutil.lib` (Apache-2.0; review in
  OA's `research/security-reviews/npu-toolchain-iron-1.4.3.md`).
- `build_kernel.ps1`: IRON / mlir-aie 1.4.3 and Peano
  22.0.0.2026090701+3e93bf7b (Apache-2.0 WITH LLVM-exception), the
  toolchain of that review, activated through its `oa_npu_env.ps1`. The
  kernel includes `aie_api` headers from mlir-aie.

The review's condition 5 still holds: nothing built from this toolchain
ships in OA until the owner decides it does.

## Build

```powershell
$n = "<toolchain folder, with oa_npu_env.ps1>"
$k = "<kit folder>"
& .\make_kit.ps1 -Toolchain $n -XrtSdk "$n\xrtsdk\xrt_sdk\xrt" -Kit $k
```

`build_kernel.ps1` compiles in `%LOCALAPPDATA%\oa-npu-cache\oa-fixed3`
(Peano's linker script splits paths at spaces). `make_kit.ps1` creates the
kit folder private to this account (owner this account, a protected DACL
of this account and SYSTEM) when it is absent, refuses one that is not,
and prints each file's SHA-256. Compiling runs nothing on the NPU.

The kit is trusted for code: whatever can write a kit file runs code in the
provider host. `npudecode` refuses a kit file, `kit.json` included, that
`trustedpath.CheckFile` (identity ID-D2) refuses with the machine's owners
(SYSTEM, Administrators, TrustedInstaller) and this account: an untrusted
owner, a write or delete right for anyone else on the file or a folder above
it, or a reparse point on its path. A kit an installer placed under Program
Files passes as one this account built does. It then reads every kit file
through a handle that denies writes and whose final path is the file's own
(no mapped or substituted drive), checks it against `kit.json` before the
shim loads, and keeps the shim's handle open until it has loaded. A caller holding `kit.json`'s own SHA-256
from a signed manifest passes it as `Config.ManifestSHA256`.

## Contract between the halves

- `fixed3.json` names the batch: `units` slots of `slot` bytes in, `units`
  units of `unit_bytes` out. `npudecode` refuses a kit whose slot is not
  `modelpack.FixedSlot` (397,376) or whose unit is not
  `modelpack.ChunkBytes` (524,288).
- The kernel's arguments are XRT's NPU convention: opcode 3, the
  instruction buffer, its word count, then the input and output buffers
  (group ids 3 and 4).
- `oa_npu_decode` returns 0, -1 on an error, -2 on a timeout. After either
  the handle refuses every later run; after a timeout `npudecode` never
  frees the buffers, which the timed-out run may still own.
- Every C entry catches every exception; none crosses into Go.
- XRT reads `xrt.ini` from `XRT_INI_PATH`, `XILINX_XRT`, the working
  directory and the executable's folder, and an `xrt.ini` can load XRT
  plugins; `npudecode` refuses to load the kit while any of them is present.
