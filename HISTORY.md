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
