# History: abstraction-inference

Linked from [CONTRACT.md](CONTRACT.md)'s "Reading this page."

## Superseded ids

Every id below is the same rule, converted from the earlier `- **INF-….**`
bullet form to the declared `**[INF-…] Title.**` form and, for most
families, moved to a new letter (`research/vocabulary/RENAME-PLAN.md`
inference step 1, `research/vocabulary/DECISION.md` S3/S4, S12):

| Old id | New id | Why the letter moved |
|---|---|---|
| INF-S1..S5 (admission) | INF-A1..A5 | `S` (admission) is `A` in every contract (S12); this frees letter `S` (below). |
| INF-V1..V3 (vision) | INF-V1..V3 | Unchanged; vision keeps letter `V`. The first-run decision's video-generation rules (below) settled on letter `G`, so no collision reaches this family. |
| INF-C1..C3 | INF-C1..C3 | Unchanged. |
| INF-E1..E3 | INF-M1..M3 | Embeddings and transcription both name the host's chat/embed/etc. sense of "profile," decided as one word, **modality** (D82); both families share letter `M`. |
| INF-T1..T3 | INF-M4..M6 | Same as above; transcription's numbers continue after embed's. |
| INF-O1..O5 | INF-Q1..Q5 | "Operation" is now **request** in prose (D2); the letter follows to `Q` since `R` already names rights. |
| INF-K1 (Clients) | INF-Q6 | `K` is reassigned to gateway keys (below); the client-wrapper rule is content about the request lifecycle and continues the `Q` family. |
| INF-P3 (`IssueKey`, `RevokeKey`, `Keys`) | INF-K2 | `K` now names gateway keys specifically, carved out of the former `P` (operator) family. Declared `INF-K1` until 2026-09-24 (below). |
| INF-P5 (`Gateway`, `SetGateway`) | INF-W9 | Folded into `W` (the gateway), since both rules are about the same surface as INF-W1..W8. |
| INF-P1, P2, P4 | INF-P1, P2, P4 | Content unchanged (P2 also carries the finding-7 fix below). The audit journal, INF-P4, was declared `INF-P3` until 2026-09-24 (below). |
| INF-H1..H3 | INF-H1..H3 | Unchanged numbers; H1 is cut to its promise (finding 11, below). |
| INF-H4, INF-H5 | retired | These were never rule paragraphs, only a pointer sentence ("moved to the facade registry"). Finding 11 asks for their deletion; the pointer is now one sentence in the "Model-server registration" section's own intro, and the facade contract (`abstraction-facade` CONTRACT.md REG-1, REG-2, REG-3, REG-4, REG-8) is cited directly instead of through a retired id. |
| INF-R1 | INF-R1 | Unchanged. |
| (none; new) | INF-S6, INF-S7, INF-L1..L5 | Declared `INF-S1`, `INF-S2` until 2026-09-24 (below). Finding 11: the un-cited "Speech and live generated output" `H1` heading becomes cited rules. The plan's own instruction named `G` for speech; the letter freed by the admission move above, `S`, carries it instead, since the first-run decision's video-generation rules (below) settled on `G`. `L` for live is unchanged from the plan. |
| (none; reserved) | INF-G1..G4 | The first-run decision (`research/first-run/DECISION.md`, `ENGINES.md` §8) reconciled its video-generation rules against this contract and settled on letter `G` (generation), keeping vision at `V`. Reserved here exactly as decided, marked not built, for 0.4.0 (Next). |

### Reused ids moved, 2026-09-24

The 2026-09-23 renumbering declared four ids that an earlier rule had
carried, so a stale citation of the earlier rule resolved against a
different one (`research/vocabulary/DECISION.md` §7 N1). S3 as amended
numbers a returning letter after the highest number it carried and lets a
moved rule keep a free old number. The four moved, and their earlier
forms are retired with both meanings:

| Declared 2026-09-23 | Now | Earlier meaning of the old id |
|---|---|---|
| INF-S1 (speech delivery) | INF-S6 | admission, now INF-A1 |
| INF-S2 (output authorization) | INF-S7 | admission's evaluation order, now INF-A2 |
| INF-K1 (minting) | INF-K2 | clients, now INF-Q6 |
| INF-P3 (audit journal) | INF-P4 | `IssueKey`, `RevokeKey`, `Keys`, now INF-K2; the audit journal carried INF-P4 before 2026-09-23 and takes it back |

The citation sweep of the same day repointed every live citation by
meaning (`research/vocabulary/RENAME-PLAN.md` §2 step 7).

### Retired ids

Every id this contract no longer declares, one per row, is never reused
(S3):

| retired | now | rule |
| --- | --- | --- |
| `INF-S1` | INF-A1; INF-S6 | admission (to 2026-09-23); speech delivery (2026-09-23) |
| `INF-S2` | INF-A2; INF-S7 | admission's evaluation order (to 2026-09-23); output authorization (2026-09-23) |
| `INF-S3` | INF-A3 | admission |
| `INF-S4` | INF-A4 | admission |
| `INF-S5` | INF-A5 | admission |
| `INF-E1` | INF-M1 | embeddings |
| `INF-E2` | INF-M2 | embeddings |
| `INF-E3` | INF-M3 | embeddings |
| `INF-T1` | INF-M4 | transcription |
| `INF-T2` | INF-M5 | transcription |
| `INF-T3` | INF-M6 | transcription |
| `INF-O1` | INF-Q1 | operations, now requests |
| `INF-O2` | INF-Q2 | operations, now requests |
| `INF-O3` | INF-Q3 | operations, now requests |
| `INF-O4` | INF-Q4 | operations, now requests |
| `INF-O5` | INF-Q5 | operations, now requests |
| `INF-K1` | INF-Q6; INF-K2 | clients (to 2026-09-23); gateway key minting (2026-09-23) |
| `INF-P3` | INF-K2; INF-P4 | `IssueKey`, `RevokeKey`, `Keys` (to 2026-09-23); the audit journal (2026-09-23) |
| `INF-P5` | INF-W9 | `Gateway`, `SetGateway` |
| `INF-H4` | none | a pointer to the facade registry |
| `INF-H5` | none | a pointer to the facade registry |

### Video generation binds, 2026-10-01

The OA side of video through an engine provider
(`research/video-demo/FINDINGS.md`) made part of the reserved `G` family
binding and added three rules. The reserved wording assumed a request that
names `server`, `template` and `preset`, with `duration_s`; the job document
released in 0.3.0 already carried `duration_ms` and names a model, and the
coordinator placed the engine behind a provider, which owns the preset.

| id | before | now |
| --- | --- | --- |
| INF-G1 | reserved: `generate` request with `server`/`template`/`model`, `duration_s`, `preset` | binding: job kind `inference` profile `video`, `duration_ms`, edit names the initial frame, preset owned by the provider |
| INF-G3 | reserved: typed reference, `path` on this machine | binding: one local delivery, MP4, WebM or AVI by the sniffed name; `path` stays Next |
| INF-G5 | none | binding: `video@1`, the executor's backend order, `request_id` held at least 24 hours after its operation ends, a `Start` naming a frame the store lacks refused with no prompt-only clip, and `native:forbidden` |
| INF-G6 | none | binding: progress and the `queue:<n>` waiting word |
| INF-G7 | none | binding: `provider:lost` for a recorded operation; a lost `Start` reply is reconciled by sending `Start` again with the same `request_id`, backing off from 1 s to 60 s, and a cancel while uncertain learns the operation and cancels it, or ends locally as `cancelled`, `upstream:unanswered`, 24 hours after the first `Start` with no answer (coordinator, 2026-10-01, before `video@1` was released) |
| INF-G8 | none | binding: the provider owns the preset; it carries the rule the reserved wording cited as the first-run decision's MODEL-K10, which no contract declares |
| INF-G2, INF-G4 | reserved | reserved, unchanged |

### Model packs, 2026-10-01

VISION 2026-10-01: OA's packs store 16-bit float weights with an in-house
exponent codec where it loads faster than the disk, and quantised weights
uncompressed. The codec measured in C++ in
`research/model-locators/GPU-AND-MATRIX-DECOMPRESSION.md` §2.2 is ported to
Go in `providerhost/modelpack` with the same substream layout, and gains a
constant plane for a unit whose exponents are all equal. Its Go
measurements are that note's §11. Before review the coordinator set one
proof granularity (2026-10-01): the index proves an object with the
SHA-256 of each `modelfiles.ChunkSize` span, the digests a section records
(INF-MS2), and the codec's 512 KiB units carry none; the decode rate is
given per dtype; the package stays beside `modelfiles` until packs become a
storage object kind.

| id | before | now |
| --- | --- | --- |
| INF-MP1 | none | binding: index of the object's length, units with offset, length, packed offset and size, encoding `stored` or `exp-huffman-v1`, and one SHA-256 per 16 MiB span equal to a section's chunk digests; an index that does not tile the object or lacks a span digest is refused before any read |
| INF-MP2 | none | binding: only `BF16` and `F16` tensor bytes are encoded, only when that dtype's decode rate exceeds the disk rate, in 512 KiB units kept only at 97 percent or less; quantised, other types and bytes between tensors stored in units of at most 16 MiB; the codec refuses other input |
| INF-MP3 | none | binding: the `exp-huffman-v1` layout: rotated values, header, raw, Huffman or constant planes, 12-bit canonical complete codes, 256 byte-aligned substreams with end offsets |
| INF-MP4 | none | binding: one positioned read per unit, decoded into the caller's memory with no file, each span checked once its units are decoded, `ErrCorrupt`, `modelfiles.ErrDigest`, parallel equals serial |
| INF-MP5 | none | binding: CPU decoder in Go; GPU decoder, pack writer, `Fill` from a pack and per-machine rate measurement not built |

### Model packs: trust anchor and portability, 2026-10-01

After the merge of the model packs above, the coordinator decided where
the proof comes from and where packs are made, before anyone builds `Fill`
from a pack. `Unpack` gained a separate argument for the span digests.

| id | before | now |
| --- | --- | --- |
| INF-MP1 | the index's `spans` are the proof of the object; size limits of a unit unstated | spans prove bytes only from OA's own record (made when OA hashed the object) or from an index bound to the object's whole SHA-256 that OA verified once; a first fill hashes the whole object and records the spans; an index read from a pack alone is never proof; an `exp-huffman-v1` unit decodes to at most 2 MiB and stores at most its decoded length plus 2,320 bytes |
| INF-MP2 | packer chooses per unit with the given rates | adds: every encoded unit is decoded before it is written and a mismatch refuses the pack; packs are made per machine at staging with its rates, stay valid when moved, and may be repacked there |
| INF-MP4 | spans checked against the index | spans checked against digests the caller passes from OA's record; spans of the wrong count or form are `ErrCorrupt` |

### Pack files and fills from a pack, 2026-10-01

The coordinator assigned the pack writer and the fill from a pack, with one
owner for OA's span record. The record lives in the provider kit beside
the fill (INF-MS7): MEMORY-LENDING.md §3 step 3 records a container's chunk
digests as INF-F2 records an engine tree, in the provider host's private
state; the span size is `modelfiles`'; and `abstraction.storage` keeps the
object's identity, which travels between machines, while a record only
proves fills on this one. `modelpack` imports `modelfiles`, so the fill
from a pack is `modelpack.FillZip` over the kit's `FillDecoded`.

| id | before | now |
| --- | --- | --- |
| INF-MS7 | none | binding: one span record per object digest in the provider host's private state, made only by OA's own hashing (a whole-object fill, the packer), malformed records ignored, the account the boundary |
| INF-MS8 | none | binding: `FillDecoded` fills a section through a decoder from a STORED region; with a record the decoder checks it and the fill trusts it; without one the fill hashes the whole object and records it; refusals free the section |
| INF-MP1 | index of length, units and spans | adds the object's `digest`, which binds the index to it; the record is INF-MS7 |
| INF-MP5 | pack writer and fill from a pack not built | built (INF-MP6, INF-MP7); no binding of the pack kind yet |
| INF-MP6 | none | binding: one zip layer, `units` STORED at a 64 KiB boundary then `index.json` STORED, two encodings compared, the packer's record given to `SpanRecords`, `ReadZip`'s refusals |
| INF-MP7 | none | binding: `FillZip` refuses an index of another digest, then fills through `FillDecoded` with no file written |

### The pack command, 2026-10-01

The owner asked for a tool that makes and reads OpenAbstractions' packs. It
is `openabstractions models pack` in the runtime's command line, a files-only
command beside the lending ones; the provider kit gained the header reader,
the streaming restore and the rate measurement it uses.

| id | before | now |
| --- | --- | --- |
| INF-MP9 | none | binding: `models pack` writes one model archive of a file or folder with the per-unit choice from given or measured rates, refuses pickles, writes a span record only into `--records`, and `info`, `verify`, `restore`, `measure`; no overwrite; exit statuses 0 to 3 |

Before the command was reviewed, the coordinator decided that OA's pack and
OA's model archive are one format (`research/model-locators/MODEL-ARCHIVE.md`
§4): the index `oa-archive.json` first, then one STORED, 64 KiB-aligned entry
per model file, under its own path when every unit stays stored. The
archive's own zip writer replaced `archive/zip`'s, which repeated the
64 KiB padding in the central directory and wrote no Zip64 field in a local
header; with Zip64 written and read in tests (forced on small archives), the
4 GiB refusal is gone.

| id | before | now |
| --- | --- | --- |
| INF-MP5 | pack writer refused units of 4 GiB or more | no such refusal; NAS store, `xor-base-v1` and tANS not built |
| INF-MP6 | pack file: `units` then `index.json`, one object, `archive/zip`'s writer, units of 4 GiB or more refused | the OA model archive: `oa-archive.json` first with files, pack indexes and tensor tables with byte and dtype-normalised digests; one entry per file, own path when stored, `<path>.oa-units` when encoded; clean unique paths; in-house writer with Zip64; tar and Expand-Archive open it |
| INF-MP7 | fill from a pack bound to one object | fill the archive's file of the object's digest |
| INF-MP9 | one pack per weight file, a folder became a folder of packs | one archive per file or folder; restore writes a file or a folder |

### The fixed exponent code, 2026-10-01

The NPU research (`GPU-AND-MATRIX-DECOMPRESSION.md` §12) measured a 3-bit
fixed exponent code at 0.712 kept and 8.45 GB/s on one CPU core in C, about
five times EH per core. The coordinator assigned it as a second encoding.
Its name follows the first: `exp-` for the exponent coded, `fixed3` for its
code, `v1` its format version.

| id | before | now |
| --- | --- | --- |
| INF-MP1 | encodings `stored`, `exp-huffman-v1` | adds `exp-fixed3-v1` with its size bound |
| INF-MP2 | a 16-bit unit is `exp-huffman-v1` where it saves 3 percent and the dtype's rate exceeds the disk's | every encoding faster than the disk is tried; the shortest expected load wins, stored included; no disk rate, no encoding |
| INF-MP5 | one CPU decoder | two CPU decoders; the NPU decoder of the fixed code stays research |
| INF-MP8 | none | binding: the `exp-fixed3-v1` unit, BF16 only, NPU chunk layout, escapes in value order, at most one per eight values |

### Archive review of 863567a43 and the escape index, 2026-10-01

The review of the archive found a zipalign extra field that overflowed its
16-bit length for paddings near 64 KiB, names that differ in letter case
only or that the index does not name, and paths Windows cannot write. The
NPU research (`GPU-AND-MATRIX-DECOMPRESSION.md` §13.6) proposed the escape
index in each chunk header so tiles can share a unit's chunks; the
coordinator adopted it into `exp-fixed3-v1`, since no pack had been written
for use, together with applying the escapes in the fill's hash pass
(§13.5 item 3).

| id | before | now |
| --- | --- | --- |
| INF-MS8 | the decoder places the bytes; the fill hashes each span | the decoder may return a function that completes each 256 KiB block of a span just before the fill hashes it |
| INF-MP2 | expected load decides | the expected load is stated as a heuristic that orders one unit's encodings |
| INF-MP6 | clean paths unique in letter case; extra field length unbounded; normalised digest "equal for the same values" | paths Windows, macOS and Linux write as given (no device names, trailing dot or space, `<>"\|?*` or control characters); entry names unique in letter case and every entry named by the index; the extra field below 65,536 bytes with zero bytes before the local header where needed; the normalised digest compares bits (NaN unquieted, `-0` ≠ `+0`), covers values alone and is compared only with its own kind |
| INF-MP7 | units checked against spans as they decode, then again by the fill | `exp-fixed3-v1` escapes applied in the fill's hash pass, which is the one check |
| INF-MP8 | chunk header bytes 1 to 63 zero | bytes 4 to 7 hold the chunk's first escape index; a decoder refuses another index |
| INF-MP9 | an existing single output was a write failure (1); outputs written in place | every existing output is a refusal (3); outputs go under a temporary name and are renamed without replacing; a failed restore removes every file and folder it made |

### A failed span record is reported on the section, 2026-10-01

The coordinator decided the safe pattern is the default: a proven fill
returns its section with no error, and a span record that could not be
written is reported on the section. Socket-root refusals wrap both
sentinels.

| id | before | now |
| --- | --- | --- |
| INF-MS7 | a failed record write unstated | leaves no record; the fill reports it on its section |
| INF-MS8 | a proven first fill whose record write failed returned its section with an error wrapping `ErrRecord` | returns its section with no error; `Section.RecordErr` wraps `ErrRecord`; one log line names the object |
| INF-EL2 | refusals wrapped `ErrSocketRoot` | wrap `ErrSocketRoot` and `privatedir.ErrNotPrivate` |

### One private-folder helper, 2026-10-01

The span record folder needed the protection the engine socket root
already had. The socket root's logic moved, unchanged, from
`enginelaunch` into `providerhost/privatedir`, and both folders use it.

| id | before | now |
| --- | --- | --- |
| INF-EL2 | root and launch directory made and checked inside `enginelaunch` | the same rules, through `providerhost/privatedir` |
| INF-MS7 | record folder created 0700 by `os.MkdirAll`, no check; the account the boundary | created and checked by `privatedir`: protected DACL of this account and SYSTEM at creation on Windows, 0700 elsewhere; a file, a link, another owner or a wider or inherited DACL refused |

### Pack fills: review of a2ceeddc0, 2026-10-01

The review asked for one reader and one proof owner, checks before a
section exists, and a refusal where the writer was untested. The record
folder's protected DACL waits for the shared private-folder helper from
the engine-transport branch.

| id | before | now |
| --- | --- | --- |
| INF-MS7 | stale `.record-` files left | removed when the folder is opened |
| INF-MS8 | the decoder's span check was the proof under a record; a failed record write refused the fill | the fill hashes every span itself after decoding; `SpanRecords` required; a proven first fill whose record write fails keeps its section with `ErrRecord`; `RegionReader`, the stored fill's unbuffered reader, one per goroutine |
| INF-MP6 | zip64 sizes in the local header at 4 GiB and over; `ReadZip` checked the digest's form only | units of 4 GiB or more refused (`errors.ErrUnsupported`); `ReadZip` checks the digest with `modelfiles` and the whole index |
| INF-MP7 | units read through one shared file handle; index checked while decoding | records required; index checked and digest binding refused before a section exists; a `RegionReader` per goroutine |

### Jobs take turns at a model server, 2026-10-01

The job sweep ran one job at a time until it ended, and a long local video
held back every other inference job in the runtime. The executor now runs
claimed jobs side by side, bounded per server, and a lease whose renewal
stalls stops its job (coordinator, 2026-10-01, after review).

| id | before | now |
| --- | --- | --- |
| INF-G6 | `queue:<n>` and `queued` waiting words | adds `host:busy` for a job the runtime holds back (INF-G9) |
| INF-G9 | none | binding: one job at a time per local server and four per hosted service by name, `host:busy`, cancel while waiting sends nothing, planned again at its turn, `service:stopped` at shutdown, recovered work counts without waiting, renewal deadline of one lease duration that bounds every wait on a renewal, publish and shutdown included (a hung store call returns in the background), per-runtime count, budget overshoot by the jobs in flight |

### Image edit mask, 2026-10-01

The image contract named an edit's mask without saying what it marks, and
OpenAI-style edits mark the area with transparency while `sd-server` reads
white, so one mask could be read inverted. VISION 2026-10-01 (request input
under input profiles; mask transparency) decided the meaning, and the image
models review asked for the rule. Letter `I` is new on this page.

| id | before | now |
| --- | --- | --- |
| INF-I1 | none | binding: a mask is a PNG of the input's pixel size and fully transparent pixels mark the area to change; inference refuses another size `invalid` `mask` and decodes the mask under the sending route's bound (Stability's documented edit limit on its route, stable-diffusion.cpp's on an OpenAI-compatible route and an image engine; beyond it `unsupported_feature` `size:engine-bound`), refusing an undecodable mask or one that marks nothing to change `invalid` `mask`; the native relay compares sizes and its receiving provider decodes under its own bound, the remote relay leaves the checks to the remote runtime; each route has one declared form from the provider kit's `imagemask` (OpenAI-compatible: alpha form also white on the area; Stability: luminance; native and remote relays: the contract mask by digest; gateway upload: already the contract form), each tested; a provider converts again for its engine (re-review, 2026-10-01) |
| INF-SD3 | forwarded fields `model`, `prompt`, `n`, `size`, `response_format`, `image`, `mask` | adds `seed` (an integer 0..2147483647 from `Config.ImageFields`) and names the provider's mask conversion |

### Engines leave TCP ports, 2026-10-01

The owner asked that engines stop raising firewall prompts and use OA IPC
(VISION 2026-10-01, "Engines adopt model bytes…; engines OA launches move
off TCP ports to endpoints only OA can reach";
`research/model-bytes/ENGINE-TRANSPORT.md` stage 1). `llama-server` b10825
already serves AF_UNIX when `--host` ends in `.sock`, Windows included.

| id | before | now |
| --- | --- | --- |
| INF-EL2 | loopback only: the tree holds its TCP listener on 127.0.0.1 at the chosen port and no socket beyond loopback | an engine that can serve a socket path serves it in a per-launch directory only this account and SYSTEM can open, the client binds the socket's server process to the engine's tree before the first byte (identity ID-T8), and the tree owns no TCP listener or bound UDP socket on any address; the directory is removed after exit and stale ones are swept. The loopback rule stays for `sd-server`, `whisper-server`, Kokoros, `lemond` and its engines, and TheNoise until each serves a socket path |
| INF-EL2 (review of the same day) | launch directories under the provider's state; listeners and UDP sockets checked at readiness; any directory with a dead socket swept | a fixed per-user socket root (`%LOCALAPPDATA%\oa\s`, `$XDG_RUNTIME_DIR/oa` or `/tmp/oa-<uid>`, `$TMPDIR/oa`) created private and refused for another owner, a wider ACL or mode, a file or a link (coordinator's decision); no TCP socket in any state and no UDP socket, outbound included, at readiness and at least every 30 seconds (coordinator's decision); a provider holds a `held` file in each launch directory and the sweep keeps held ones; one call, `CheckEngineSockets`, applies the socket or the loopback rule by the engine's transport |

### Model memory, 2026-10-01

VISION 2026-10-01: OA fills a model's bytes into a memory section once and
lends it read-only to engines. The design and prototype are
`research/model-locators/MEMORY-LENDING.md`; the provider kit gained
`providerhost/modelfiles` beside `enginefiles`. The prototype's measurement
of the default and protected descriptors is repeated as a test with an
engine stand-in. The prototype's parallel decode of a chunked pack is not
carried over: OA's packs stay uncompressed, and no chunked pack format
exists.

| id | before | now |
| --- | --- | --- |
| INF-MS1 | none | binding: a pagefile-backed section filled from a file, a STORED region or one DEFLATE stream, with no file written |
| INF-MS2 | none | binding: the object's `sha256:` digest is required and must decode as hex, every placed byte is hashed (whole object and 16 MiB chunks), a DEFLATE stream's length and CRC-32 are checked, and any refusal frees the section |
| INF-MS3 | none | binding: the writable view is dropped and the provider host's own handle becomes map-read and query only after the proof; protected DACL `D:P(A;;0x5;;;OW)`; every loan read-only, no writable loan; an engine can copy its handle within the account |
| INF-MS4 | none | binding: one read-only inheritable duplicate per engine through supervision's handle list (facade FAC-P1); the lease ends on confirmed exit or reported release, and at once by `End` when the engine does not start |
| INF-MS5 | none | binding: no warm rule; freed at the last loan's end or the owner's close with none open; the end that frees the section refuses a racing `Lend`; the memory holding's release is called once |
| INF-MS6 | none | binding: Windows only; Linux sealed `memfd` stated, not built; no engine at its pin reads an inherited handle, so no launch lends yet |

### Jobs ask admission, 2026-10-02

The first video job sent through an isolated runtime to a provider declared
on demand waited `upstream:uncertain` with the provider never started
(`research/live-runs/2026-10-02.md`, phase 2 finding 1). Every `Start` path
asked the provider's admission hook, where the runtime starts an on-demand
provider; the job executor sent work to the picked server without asking.
Both now ask through one function (`admitHost`).

| id | before | now |
| --- | --- | --- |
| INF-A5 | run for bounded `Start` operations and `Embed` | also run by the job executor before any call reaches a job's server, first submission and recovered work alike; any word but `not_permitted` waits with that word, `not_permitted` fails `admission:not_permitted` |

### A job outlives its server's declaration once, 2026-10-02

The same run's job, accepted before its provider's declaration was replaced,
waited `host:replaced` until it was cancelled, and a job whose server was
withdrawn waited `host:unavailable` without end (phase 2 finding 2). The
coordinator decided: plan afresh once, fail with a clear word when the new
declaration cannot serve the job, and never wait without end.

| id | before | now |
| --- | --- | --- |
| INF-G10 | none: a replaced binding waited `host:replaced` and a gone server `host:unavailable`, both without end | binding: a replaced server, or one gone for 30 minutes, plans a credential-free job afresh once with its same `request_id` and fails it with the router's reason when none serves it; a second change or a credentialed job fails `host:replaced` or `host:gone`; `host:unbound` unchanged |
| INF-G10 | the 30 minutes counted from the first time the server was found gone, kept after it answered again | counted from the start of the current outage; a server that answers again clears it (review of fa380274e) |

### A job asks for every grant at once, under its own output profile, 2026-10-02

The same run's application needed six hand-written rules for one video job,
each learnt from a separate refusal, and its clip was authorized as
`output:image` (phase 2 finding 4). Coordinator's decision: a video output
profile end to end; one refusal naming every missing grant, admitting all
their first-use questions in one pass; output pairing that reaches only the
program's own results; the inference bundle adds job submission and the
output pairs it names (facade `go/grants`).

| id | before | now |
| --- | --- | --- |
| INF-S7 | `output:speech`, `output:live`, `output:image`; no question for an output | adds `output:video` for a video job's clip; a job refused for its output asks the paired question (`Config.AskOutput`), whose read reaches only the program's own results |
| INF-V4 | a job answered the first refusal it met | a job decides `complete`, its input and its output before answering and is refused once, naming each missing grant in that order, with every first-use question admitted in the same pass |

### A synchronous output asks too, 2026-10-02

`research/wake-and-inference-rights/REPORT.md` recommendation 7: an
`image@1` or `speech@1` call refused for want of its output profile asked
nothing, so OpenCode's image tool and Speak needed hand-made rules. The
runtime now admits the paired question where it authorizes every generated
output (serve's content preparer), for calls and jobs alike, and
`output:speech` pairs. The call is refused at once and succeeds on retry,
as a job is.

| id | before | now |
| --- | --- | --- |
| INF-S7 | a job refused for its output asks the paired question on `output:image` or `output:video` (`Config.AskOutput`) | a synchronous call or a job refused for its output asks the paired question on `output:image`, `output:speech` or `output:video`, admitted where the output write is authorized; `Config.AskOutput` stays for hosts whose preparer does not ask |

### A job's grant refusal reads forbidden, 2026-10-02

The live walkthrough of 2026-10-02 (D11) saw a video job refused for want
of `output:video` answer `invalid output:forbidden`: JOB-A16 then allowed
only `invalid`, so `CheckAdmissionForSubject` mapped its rights refusal to
`invalid`. JOB-A16 now admits `forbidden` without sealing the key.

| id | before | now |
| --- | --- | --- |
| INF-V4 | a job refused at submission for its grants reads `invalid` with the combined reason | it reads `forbidden` with the combined reason, unsealed, so a retry after the answers is accepted; a credential refused other than `not_permitted` reads `invalid` |

### Steps under way are read every second, 2026-10-02

The live walkthrough of 2026-10-02 (D10) saw a video job's progress skip a
step: the executor read the provider every 5 s and an F32 step took about
5.8 s. `video@1`'s `Observe` has no wait for a change, and adding one is a
wire change, so the executor reads a provider that reports steps under way
every second and keeps 5 s for queued or silent work (`pollWait`), which
leaves a remote API's request rate unchanged.

| id | before | now |
| --- | --- | --- |
| INF-G6 | progress reaches the snapshot at the executor's 5 s poll | steps under way are read every second; queued or silent work every five seconds |
### A synchronous call names every missing grant at once, 2026-10-02

The first use of an image@1 or speech@1 call still took two refusals: the
`complete` question came first, and the output question only on the next
try, while a job asked both at once. The grant decisions now have one owner
in the provider (`grantRefusal`, `decideComplete`, `prepareOutputs`), which
the job executor and both calls use.

| id | before | now |
| --- | --- | --- |
| INF-V4 | a durable job decides every grant before it answers and is refused once, naming each | an image@1 or speech@1 call does the same: refused once (`not_permitted` when `complete` is among them, else `forbidden`), every first-use question admitted in the same pass; only a `not_granted` complete gathers the others, a denied complete or an unknown action refuses with that word alone and asks nothing, for a job as for a call; a decision word other than `permitted`, `denied`, `not_granted` or `unknown_action` reads `unavailable` for a job as for a call |

### One GPU library path gate, 2026-10-02

The PyTorch-on-ROCm stack review's condition 5 asks for TheNoise's path
gate before every GPU launch of the model host's PyTorch engine
(`research/security-reviews/pytorch-rocm-windows-2.11.0-rocm7.13.0.md`;
review of `779d7f0c2`). TheNoise's `gpugate.go` moved into `enginelaunch`
as `GPUPathGate`, with its Windows probes; TheNoise calls it with its
bundle's two folders and keeps its own refusal sentence. `sdcpp.CheckPrompt`
exports the prompt bound every request body applies, for an engine that
builds no sd.cpp body.

| id | before | now |
| --- | --- | --- |
| INF-EL3 | TheNoise's own gate (its review's conditions 8 and 9) | binding: one kit gate for every launch that loads hipBLASLt or rocBLAS kernel files on Windows; TheNoise and the model host's PyTorch engine apply it |
| INF-IO1 (FLUX.2 [klein] on the model host's PyTorch engine, coordinator 2026-10-02) | none; each image provider received a finished image from its engine, and `providerhost.NewEngine` held the output publication inline | binding: `providerhost/rgbpng` writes an engine's RGB24 pixels as PNG with Go's encoder; `DesignatedContent.OutputWriter` is the one generated-output preparer, used by `NewEngine` and the model host's image kind |

### The NPU decodes fixed-code units in a fill, 2026-10-02

The owner asked to look into fast and efficient NPU decoding and approved
NPU work in OA for this step; the coordinator assigned the NPU as an
optional decoder of `exp-fixed3-v1` units in the fill, with the CPU decode
as reference and fallback. The design follows
`GPU-AND-MATRIX-DECOMPRESSION.md` §13.5 with escapes in the hash pass and
the output copied into the section; measurements in §14.

| id | before | now |
| --- | --- | --- |
| INF-MP5 | the NPU decoder of the fixed code stays research | an NPU decodes `exp-fixed3-v1` units inside a fill (INF-MP10) |
| INF-MP10 | none | binding: `FillZipWith` with an optional `FixedDevice`, used only after `CheckFixedDevice`; units checked on the CPU before each run; escapes in the hash pass; a device error hands that batch and the rest to the CPU and stops the device; the fill's proof unchanged, and a refusal after device output repeats the fill on the CPU and distrusts the device when that fill is proven; `npudecode` loads OA's XRT shim without cgo, one batch at a time, 500 ms per run |

Review of f5d8a4045 (2026-10-02) found the proof sound and asked for a
trusted kit: the shim loaded with its imports from System32 alone, the kit
folder private and its files pinned, XRT configuration refused, `Close`
safe during a fill, catch-all handlers at the C entries.

| id | before | now |
| --- | --- | --- |
| INF-MP10 | shim loaded from its folder and System32; kit files read as found; `Close` freed at once | the kit folder is trusted for code: private to this account (`privatedir.CheckDir`) and, after the merge of the model-folder rules, refused by `CheckModelsRoot`'s rules for a folder OA owns (no link or junction at or above it, resolving to itself); every file read through a write-denying handle and checked against `kit.json`'s SHA-256 (optionally pinned by the caller), the shim loaded by absolute path with System32-only imports while its handle is open; `XRT_INI_PATH`, `XILINX_XRT` or an `xrt.ini` in the working directory or beside the executable refused (`ErrXRTConfig`); `Close` waits for the fill holding the device; every C entry catches every exception; an archive rewritten between a fill and its CPU rerun can distrust a healthy device (accepted) |

The review of the kit check (2026-10-02) found that judging the folder
alone let a kit file with a wider entry of its own pass: Everyone granted
Modify on the shim and on `kit.json` still reached `LoadLibrary`. The
trust rule is now identity ID-D2 per file.

| id | before | now |
| --- | --- | --- |
| INF-MP10 | the kit folder private (`privatedir.CheckDir`) and passing `CheckModelsRoot` | every kit file, `kit.json` included, passes ID-D2 (`trustedpath.CheckFile`, the machine's owners and this account), which also refuses reparse points on the whole path and admits an installer's kit under Program Files; each file's open handle must resolve to its own path, the one check of `CheckModelsRoot` ID-D2 does not make |

### The escape pass visits recorded positions, 2026-10-02

The fill's escape pass tested every value and cost about as much as
SHA-256 (`GPU-AND-MATRIX-DECOMPRESSION.md` §14.3). The coordinator decided
that the CPU decoder and the device's check record each escape's position,
and that the pass applies escapes there alone, with the proof unchanged.

| id | before | now |
| --- | --- | --- |
| INF-MP7 | the pass tests every value for the mark `b + 7`; the fill holds bases and escapes, at most one byte per sixteen | the decode records each escape's position within its chunk and each chunk's first escape (the header's index); the pass writes only there and only over the mark; a wrong list leaves bytes the hash refuses; the fill holds bases, escapes and positions, at most 3/16 of the decoded size plus 5 bytes per 4,096 values (a hostile pack's bound), and drops a unit's once every span holding it is passed |

### Video encoding, 2026-10-02

A PyTorch engine returns raw frames, and the Motion-JPEG AVI the model host
built from them plays in neither browsers nor most players. Decided by the
coordinator: H.264 in MP4 through Media Foundation, which ships with
Windows, bound in-house over `golang.org/x/sys/windows` with no cgo, in one
kit package every engine kind shares; no fallback encoder elsewhere.
Measured on the owner's machine with Microsoft's software encoder: twelve
256x144 frames at 16 fps wrote `ftyp`, `uuid`, `mdat`, `moov` with 12
samples over 0.75 s, and three frames decoded back through the Source
Reader at 39.9 to 40.0 dB PSNR against their sources.

| id | before | now |
| --- | --- | --- |
| INF-VE1 | none | binding: `providerhost/videoenc` encodes RGB24 or RGBA frames to H.264 High in MP4 through the Sink Writer, hardware encoder when present and not refused, software otherwise, the result naming which; other platforms answer unsupported |
| INF-VE1 (review of `8abe84047`) | Abort could close the call channel under a live Write (a panic); concurrent Writes could share a frame index; any bit rate passed; the writer replaced a file at the path; moov came last | the call channel is never closed and the thread ends on its own signal; the frame index is taken on the encoder's thread; a bit rate above 2^32 - 1 is refused; a temporary name in the same folder moves into place without replacing anything (`serve`'s no-replace rename moved to `providerhost/noreplace`), an existing path refused; moov before mdat by the kit's own fast-start rewrite, since `MF_MPEG4SINK_MOOV_BEFORE_MDAT` failed Finalize with E_ACCESSDENIED on the writer's own file and wrote a corrupt layout on a read-write `MFCreateFile` stream |
| INF-VE1 (review of `0410d167f`) | a chunk offset that passed 4 GiB once moov moved failed fast start, and Close then deleted the finished video | every `stco` becomes a `co64` when an offset would pass 32 bits, the shift taken from the final moov size (`TestFastStartWidensOffsetsPastFourGiB`, a 4 GiB file read and written sparse); any other fast-start failure places the encoder's own file, moov last, with `FastStart` false (`TestFailedFastStartPlacesTheEncodersFile`) |

### OA's rANS unit, 2026-10-02

VISION 2026-10-02 decided an in-house range/ANS coder as a third
encoding, measured before defaults are chosen; `research/model-locators/ANS.md`
has the measurement on the Qwen2.5-1.5B pair.

| id | before | now |
| --- | --- | --- |
| INF-MP1 | encodings `stored`, `exp-huffman-v1`, `exp-fixed3-v1` | adds `exp-ans-v1`, stored in at most its length plus 16 bytes |
| INF-MP5 | tANS units not built | `exp-ans-v1` decodes wherever a pack is read; `Pack` does not write it yet; the delta unit kind is not built |
| INF-MP11 | none | binding: the `exp-ans-v1` unit: two byte planes, raw, constant, rANS with one 12-bit table, or (delta layout) one table per base exponent in a window of at most 32; four interleaved 32-bit states over 16-bit words, each ending at 2^16; refusals |

### Delta units against a proven base, 2026-10-02

VISION 2026-10-02 (1) to (3): the archive stores a fine-tune as an exact
integer difference against a proven base; `research/model-locators/ANS.md`
chose `exp-ans-v1` with base-exponent contexts as its coding.

| id | before | now |
| --- | --- | --- |
| INF-MP1 | units without a base | a unit may carry `transform` and `base_offset`; an index holding delta units names its `base` |
| INF-MP5 | the delta unit kind not built; `Pack` writes no `exp-ans-v1` | `Pack` writes `exp-ans-v1` as delta units; plain weights keep `exp-huffman-v1` |
| INF-MP12 | none | binding: `int-diff-v1` delta units coded `exp-ans-v1` under the base's exponents, the undo fused into the plane join; index rules; `PackDelta` and `--base`; the base proven before any delta unit decodes (`ProvenBase`, `FillOptions.Base` through a loan), refused before decoding when missing (`ErrBase`) or another object (`modelfiles.ErrDigest`); the fine-tune proved as every fill is; retention left to the store that keeps archives (`ModelArchive.Bases`) |

### preload@1, 2026-10-03

The measured cold first answer of OpenCode's Local assistant on
Qwen3.6-35B-A3B was 10.6-12 s, against 3.6 s with its fixed prefix
evaluated ahead (`research/live-runs/2026-10-02-demo-model-opencode.md`).
The coordinator decided that the application supplies its own prefix and
providers learn nothing (review finding F1 of the modelhost warm: a prefix
learned from one caller and evaluated for another is an oracle through
`cached_tokens`). Service `abstraction.inference/preload@1`, method
`Preload(Request) PreloadReply`; the word is Ollama's "preload a model"
(the Ollama FAQ that `research/vocabulary/DECISION.md` §1 reads), since
`prepare` is the job executor's.

| id | before | now |
| --- | --- | --- |
| INF-PL1 | none | binding: the request is the caller's own chat@1 prefix, last message not the user's; nothing kept |
| INF-PL2 | none | binding: Start's admission order; served by a provider's prefix evaluator (`Config.EvaluatePrefix`) or a native provider that declares preload@1 (`router.Host.Preloads`); `unsupported_feature` otherwise; the evaluator gets the chat path's `openAIBody`; the lowest standing: a marked admission (`PreloadAdmission`) that never asks a holder to yield (`busy` `resources:held`) and no model activity during the evaluation |
| INF-PL3 | none | binding: outcomes `evaluated`, `loaded`, `busy` (`engine:used`, `preload:running`, `resources:held`), `refused` and Start's refusals but `budget_exceeded`, which a preload never reaches; the 35 s call budget |
| INF-PL4 | none | binding: no widening of the shared-cache exposure; audit records of modality `preload` |

### A program reads its own selection policies, 2026-10-07

The OpenCode plugin reads its `oa-chat-default` policy, which the Panel's
per-app chooser writes, to list it and make it OpenCode's model (adopter
boundary move plan step 5). `selection.manage` gated that read, and
installation grants it to no application, so a stock OpenCode read
`forbidden`. The coordinator decided under VISION 2026-09-17 (ownership
scopes a program's own records) that a program reads its own policies with
no grant. The self scope is the connection's bound program, compared by
`identity.SameSubjectProgram`, never by the target's spelling.

| id | before | now |
| --- | --- | --- |
| INF-R1 | `selection.manage` gates selection-operator@2 `Read` of the caller's own scope | that `Read` needs no grant; `Put` and `DeletePolicy` keep `selection.manage`; another program's scope keeps `selection.manage_for` for every call; selection-operator@1 is unchanged |

## Intermediate selection profile retired before publication, 2026-10-09

The unpublished `selection-operator@1` profile and its three methods were
removed after auditing OpenCode, Panel and Python callers, which use
`selection-operator@2`. Pinned and unpinned policies retain the same persisted
store and revision format. Own reads retain their bound-program scope and need
no grant; edits retain `selection.manage`, and another program's reads and
edits retain `selection.manage_for`. Published Chat and Image version 1 remain
unchanged. The generated bindings and permission reference now describe the
remaining profile.

## Design records

- `research/vocabulary/DECISION.md` D1, D2, D4, D6, D9, D10, D11, D15, D16,
  D20, D30, D42 (unapplied here; see below), D51, D59, D60, D64, D82, D83,
  D87, S1-S13 — the contract shape and the words this release applies:
  request (not operation), service (not wire contract), module (not layer),
  provider (implementation only), the registry's role values kept on the
  wire, model server (not host, except the URL sense), placement is
  facade's word and not repeated here, subject, location, deprecated,
  outcome, gateway key, the gateway, budget, modality, hosted server (not
  the upstream sense of provider), the `unavailable` reason families for a
  provider's own refusal.
- `research/vocabulary/RENAME-PLAN.md` inference steps 1-15 — the per-step
  plan this release applies in full.
- `research/inference/DECISION.md` — VISION 2026-09-16, "Hosted model calls
  are an inference seat," the decision this contract implements.
- `research/inference-modalities/DECISION.md` — the decision for `embed@1`
  as one bounded call, and for video as job kind `inference` carrying an
  `image@1`-shaped request with `duration`.
- `research/inference-modalities/LIVE-MEASUREMENT-2026-09-22.md` — the
  gateway session latency measurement, below.
- `research/inference-registration/DECISION.md`,
  `research/provider-registry/DECISION.md` — the decisions behind
  model-server registration as a facade registry declaration.
- `research/first-run/ENGINES.md` §6, §8 — ComfyUI as a graph-kind model
  server, and the `INF-G1..G4` rules reserved for it (Next section).
- VISION 2026-10-01, "Content an application uploads as request input is
  written and read under a bounded input profile…" — `INF-V4` (2026-10-01).
  Edit inputs, masks, first frames and dictations had needed exact-digest
  write and read rules that no Panel button grants, and stayed in the store
  indefinitely. The input profiles mirror INF-S7's output profiles; the
  storage side is `abstraction-storage` STO-W11, STO-W12 and STO-T7.
  Revised the same day: the coordinator decided the one-ask pairing (a
  first-use question on an input profile's write is admitted only with its
  consuming request, and its answer writes both content rights), and the
  review added the profile check on read-back and the rule that an
  undecodable live job keeps every object.

## Reviewed and applied

`research/reviews/inference-facade-2026-09-23.md`, findings against
`abstraction-inference`:

- **Finding 1** (spend units stated three ways) — INF-C2 now names every
  enforced unit in one place, grounded in `go/ceiling.go`'s `Exceeded` and
  the five per-modality test files that each admit over one unit's budget.
- **Finding 2** (one credential's daily cap in two records) — the
  "Model-server registration" intro and INF-H3 now say the budget lives in
  the model server's own declaration alone; `hosts.json` keeps only the
  hosted server's own bounds and the `declared` switch.
- **Finding 3** (`profile` carries two senses) — applied as the D82 rename,
  `modality`, throughout INF-H2, H3, P2, M1, M4, M6.
- **Finding 4** (three words for the spend cap) — applied as the D64 rename,
  `budget`, in prose; `budget_exceeded` and `budget.manage` needed no change,
  already this word on the wire.
- **Finding 5** (`gateway window`, `window`, `local key`) — applied as
  `the gateway` and `gateway key` throughout, keeping the `INF-W` letter and
  the wire kind `openabstractions/local-key@1`.
- **Finding 6** (`provider` names both the upstream vendor and the
  implementation) — applied as `hosted server` for the upstream sense
  (INF-V2, the "Model-server registration" intro) and `provider` kept for
  the implementation sense throughout.
- **Finding 7** (INF-P2 against INF-H3 on whether `Hosts` writes) — INF-P2
  now says the pairing's `apply` rule is written by whichever edit creates
  it, `AddHost` or the credential's own `Store`, and `Hosts` only reads;
  INF-H3 cross-references INF-P2 instead of repeating the claim.
- **Finding 8** (the credential consumers seed and the gateway key's
  consumer excluded embed) — INF-K1 and INF-W5 now say the gateway decides
  a key's consumer per route: `chat@1` for chat, `embed@1` for
  `/v1/embeddings`, `live@1` for `/v1/realtime`. The credentials-side half
  of this finding (the Consumers seed and the worked `credentials add`
  example) was applied by `abstraction-credentials` already; see its
  HISTORY.md.
- **Finding 11** (rule shape) — the "Speech and live generated output" `H1`
  becomes cited `INF-S` and `INF-L` rules; `INF-H4`/`H5` retired (table
  above); `INF-H1` cut to its promise, with the five-product table moved to
  README.md; `INF-S5` (now `INF-A5`) says "after step 3" instead of naming
  step 2 and leaving step 3 ambiguous.

## Reviewed and not changed here

- **Finding 9** (facade `Scope` is placement) and **finding 10** (facade
  rule shape) name `abstraction-facade` rules only; nothing changed here.
- `providers review` (`research/reviews/providers-2026-09-23.md`) finding 2
  (`HOST-3`/`HOST-6`/`HOST-14` reason families) names
  `abstraction-provider-modelhost` rules; INF-A2 already states the
  `lease:<outcome>`, `engine:none` and `object:unknown:<store>/<id>` reason
  families a provider's own refusal reads as, per D87. No further change
  was needed here for this release.

## Not this worker's to make

Out of scope for this worktree, named in the rename plan's own rows but
owned by other modules or steps: `ref:145`, `ref:270-278` and the Panel's
inference captions (site and Panel, plan step 4, "Panel and command line");
`serve/inference.go`, `serve/runtime_rights.go` and `monitor/inference_page.go`
(CLI flags, the rights-action dual reader, and Panel labels); the stored
policy's dual reader for `inference/complete` -> `inference/chat.complete`
and for `host:<name>` -> `server:<name>` (rights' stored policy); the
`operator@2`, `chat@2` and `registry@2` service versions themselves
(RENAME-PLAN.md §4, scheduled by release, not by module).

The id renumbering above leaves stale citations of the retired forms in
`go/provider.go` and `go/gateway/live.go` comments (this module, not touched
because the plan's rows name no file under `abstraction-inference/go/`), and
in `abstraction-asks`, `abstraction-credentials`, `abstraction-facade`,
`abstraction-provider-modelhost` and `abstraction-router` files owned by
those modules. Flagged as a follow-up task rather than edited here. A
`[INF-S1]` or `[INF-S2]` citation among them is the sharper case: the old
admission family retired those exact forms, and the new speech family
(above) now declares them, so a stale citation of the old meaning resolves
silently against the new rule instead of failing the tags gate. The
follow-up sweep needs to repoint each one at its real target, `INF-A1` or
`INF-A2` for the admission sense, not merely confirm it still resolves.

## Measured

`serve/runtime_live_voice_measurement_test.go` measures one gateway
session's added latency against the same fixture reached directly.
Hosted-server interoperability is unmeasured.

## 2026-10-01: the private-folder helper moved to identity

`providerhost/privatedir` moved to abstraction-identity as `go/privatedir`
(identity ID-D1) with no change of behaviour, when the storage reader's
digest record (storage-over-local-stores LS-12) became its fourth caller.

| id | before | now |
| --- | --- | --- |
| INF-EL2, INF-MS7 | made and checked through `providerhost/privatedir` | the same rules, through abstraction-identity's `go/privatedir` (ID-D1) |

## 2026-10-07: admitted work lives only while its rules do

A survey after the wake revoke fix (abstraction-resource RES-A1) found that
a revoke or Block stopped only the next `Start`. A live session was decided
once at `StartLive` and kept appending, and a running durable job kept
polling with the headers its plan applied. Every other running operation
also streamed on to its end (TODO "Live grants outlive revocation"). INF-A6
now decides that work again, asking nobody, through `Config.Standing`
(`go/standing.go`): before each live input, before each job poll, on every
committed rights change (abstraction-rights RIGHTS-R23) and every five
seconds on the watchdog. The runtime's `Standing` decides `complete` on the
server and `abstraction.credentials/apply` on the credential it applied.
Without `Standing`, a leaf provider decides `complete` through its static
`Decide`. Tests: `go/standing_test.go` (revoked live input closed and the
session cancelled with `rights:denied`; an outage sends nothing and
recovers; `Recheck` ends a running session with the credential refusal and
leaves it through an outage; a revoked job polls no more, cancels its
upstream work and fails `rights:denied`) and the runtime's
`serve/runtime_standing_test.go` (the standing decides both rules, asks no
question, and each committed change rechecks the provider until unhooked).
`TestInferenceJobDecidesWithTheSubmittersPeer` now also expects each poll's
decision to carry the submitter's peer.

| id | before | now |
| --- | --- | --- |
| INF-A6 | none: rights and credential decided once, at `Start` or a job's plan | decided again on each live input, job poll, committed rights change and five-second clock; a refusal ends the work |
