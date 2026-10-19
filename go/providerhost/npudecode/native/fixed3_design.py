"""IRON design of OA's NPU decoder of exp-fixed3-v1 units (build time only).

Compiles fixed3_decode.cc into one xclbin and its instruction stream for a
batch of `units` units of 512 KiB of BF16, each in a slot of `slot` bytes
(the most an exp-fixed3-v1 unit of 512 KiB takes: 64-byte header, 64 chunks
of 5,696 bytes, 32,768 escape bytes). The runtime sequence reads only the
chunks of each slot, skipping the unit's header and escapes, on `cols`
shim streams with one compute tile each, and writes the decoded units
contiguously, 512 KiB per slot. Escaped values leave with exponent b + 7.

This compiles; it never runs anything on the NPU. It writes fixed3.xclbin,
fixed3.insts and fixed3.json (the batch shape the Go loader checks) into
--out, a folder whose path has no space (Peano's linker script splits paths
at spaces).

  python fixed3_design.py --out <a folder with no space, such as %LOCALAPPDATA%\\oa-npu-cache\\oa-fixed3>
"""

import argparse
import json
from pathlib import Path

import aie.iron as iron
import numpy as np
from aie.helpers.taplib.tap import TensorAccessPattern
from aie.iron import CompileTime, ExternalFunction, In, ObjectFifo, Out, Program, Runtime, TaskGroup, Worker
from aie.iron.controlflow import range_
from aie.utils.config import cxx_header_path

HERE = Path(__file__).resolve().parent

VALS = 4096
IN_CHUNK = 64 + 3 * 512 + VALS  # 5,696
OUT_CHUNK = 2 * VALS  # 8,192
UNIT_VALS = 256 * 1024  # 512 KiB of BF16, modelpack.ChunkBytes
UNIT_CHUNKS = UNIT_VALS // VALS  # 64
UNIT_OUT = 2 * UNIT_VALS
SLOT = 64 + UNIT_CHUNKS * IN_CHUNK + UNIT_VALS // 8  # 397,376, modelpack.FixedSlot
HALF = IN_CHUNK // 2  # 2,848 bytes: one DMA dimension stays under 1,024 words


@iron.jit
def fixed3_units(units: In, dec: Out, *, n_units: CompileTime[int], slot: CompileTime[int], n_cols: CompileTime[int]):
    ups = n_units // n_cols  # units per stream
    per_tile = ups * UNIT_CHUNKS
    in_ty = np.ndarray[(n_units * slot,), np.dtype[np.uint8]]
    out_ty = np.ndarray[(n_units * UNIT_OUT,), np.dtype[np.uint8]]
    in_tile = np.ndarray[(IN_CHUNK,), np.dtype[np.uint8]]
    out_tile = np.ndarray[(OUT_CHUNK,), np.dtype[np.uint8]]
    kern = ExternalFunction(
        "fixed3_decode",
        object_file_name="fixed3_decode.o",
        source_file=str(HERE / "fixed3_decode.cc"),
        arg_types=[in_tile, out_tile],
        include_dirs=[cxx_header_path()],
    )
    f_in = [ObjectFifo(in_tile, name=f"u{s}") for s in range(n_cols)]
    f_out = [ObjectFifo(out_tile, name=f"d{s}") for s in range(n_cols)]

    def core_body(fi, fo, k):
        for _ in range_(per_tile):
            a = fi.acquire(1)
            b = fo.acquire(1)
            k(a, b)
            fi.release(1)
            fo.release(1)

    workers = [Worker(core_body, [f_in[s].cons(), f_out[s].prod(), kern]) for s in range(n_cols)]
    in_taps = [
        TensorAccessPattern((1, n_units * slot), s * ups * slot + 64, [1, ups, 2 * UNIT_CHUNKS, HALF], [0, slot, HALF, 1])
        for s in range(n_cols)
    ]
    out_taps = [
        TensorAccessPattern((1, n_units * UNIT_OUT), s * ups * UNIT_OUT, [1, 1, 1, ups * UNIT_OUT], [0, 0, 0, 1])
        for s in range(n_cols)
    ]
    prods = [f.prod() for f in f_in]
    conss = [f.cons() for f in f_out]

    def sequence(a, c, ps, cs):
        tg_in = TaskGroup()
        for s in range(n_cols):
            ps[s].fill(a, in_taps[s], group=tg_in)
        tg_in.finish()
        tg_out = TaskGroup()
        for s in range(n_cols):
            cs[s].drain(c, out_taps[s], wait=True, group=tg_out)
        tg_out.finish()

    rt = Runtime(sequence, [in_ty, out_ty, prods, conss])
    return Program(iron.get_current_device(), rt, workers=workers).resolve_program()


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--out", required=True, help="output folder; its path must have no space")
    ap.add_argument("--units", type=int, default=64, help="units per batch, a multiple of --cols (default 64)")
    ap.add_argument("--cols", type=int, default=8, help="shim streams and compute tiles (default 8)")
    o = ap.parse_args()
    out = Path(o.out).resolve()
    if " " in str(out):
        raise SystemExit(f"{out}: the path has a space, which Peano's linker script splits")
    if o.units <= 0 or o.units % o.cols:
        raise SystemExit("--units must be a positive multiple of --cols")
    out.mkdir(parents=True, exist_ok=True)
    xclbin, insts = out / "fixed3.xclbin", out / "fixed3.insts"
    fixed3_units.specialize(n_units=o.units, slot=SLOT, n_cols=o.cols).compile(xclbin_path=xclbin, inst_path=insts)
    shape = {
        "format": "oa-npu-fixed3",
        "version": 1,
        "kernel": "MLIR_AIE",
        "units": o.units,
        "slot": SLOT,
        "unit_bytes": UNIT_OUT,
        "columns": o.cols,
    }
    (out / "fixed3.json").write_text(json.dumps(shape, indent=2) + "\n", encoding="utf-8", newline="\n")
    print(f"wrote {xclbin}, {insts} and fixed3.json: {o.units} units per batch on {o.cols} tiles")


if __name__ == "__main__":
    main()
