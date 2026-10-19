# abstraction.inference contract

`inference.thrift` defines model calls through `chat@1`, `chat@2`, `embed@1`,
`transcription@1`, `speech@1`, `live@1`, `image@1` and `image@2`; operator
services administer hosts and scoped selection policies. `job.thrift` defines the durable document job
kind `inference` carries for video and image-batch work, executed by the same
provider. `video@1` is the service a native video provider serves to that
executor. This file states the obligations a provider meets.

Binds: `inference.thrift`

An application names a model family and the behavior it needs. The runtime
chooses an allowed local engine such as Ollama or Lemonade, or a hosted
server such as OpenRouter, then applies the named credential inside the
service. [README.md](README.md) holds the walkthrough: setup, a worked chat
call, and the gateway.

## Reading this page

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD",
"SHOULD NOT", "RECOMMENDED", "NOT RECOMMENDED", "MAY", and "OPTIONAL" in this
document are to be interpreted as described in BCP 14 [RFC2119] [RFC8174]
when, and only when, they appear in all capitals, as shown here.

The rule ids below, such as `INF-A1`, are what tests and refusals cite. A
retired id is never reused; [HISTORY.md](HISTORY.md) keeps it with the
release it left.

| Letter | Meaning |
| --- | --- |
| A | admission |
| V | image input (vision) |
| C | credentials and spend |
| M | modalities: embeddings, transcription |
| Q | the request lifecycle, including each language client |
| S | speech |
| L | live |
| H | model-server registration |
| K | gateway keys |
| W | the gateway |
| P | the remaining operator calls |
| R | the closed rights-action catalogue |
| F | the files of an engine a provider launches |
| MS | a model's bytes in a memory section a provider host lends to the engines it launches |
| PL | preloading a chat prefix ([Preload](#preload)) |
| G | video generation ([Video generation](#video-generation)); G2 and G4 reserved ([Next](#next-video-generation)) |
| I | image edits ([Image edits](#image-edits)) |
| MP | the encoding units of a model pack and its proof ([Model packs](#model-packs)) |
| VE | the video encoding of an engine's raw frames ([Video encoding](#video-encoding)) |

`E` (error and outcome) and `X` (extension) are the reserved letters of
`research/vocabulary/DECISION.md` S12; this page declares no rule under
either this release.

| Prose | Wire (until its own release) |
| --- | --- |
| model server, `server:<name>` | `host:<name>` in stored policy, and `Hosts`, `AddHost`, `RemoveHost`, `HostEntry`, `HostChange`, `HostList`, `HostState` on `operator@1`; the dual reader lands with rights' stored policy, and the `Servers`/`AddServer`/`RemoveServer` rename with `operator@2`, neither here |
| `inference server add`, `inference server list` | `inference host add`, `inference host list` (CLI, `serve/inference.go`); lands with the Panel and command-line step |
| modality, `modality:<name>` | `profile:<name>` (rights resource), `host_profiles`, `Config.Admit`'s `profile` argument, `--profiles` (CLI); `chat@2`/`registry@2` land in the plan's second and third releases |
| request | the `operation` field on `Admission`, `Reply`, `AuditEntry` and elsewhere; `chat@2` |
| budget | `CeilingLimit`, `host.ceiling`, `Ceilings`, and reason `ceiling:<unit>:<credential>`; `budget_exceeded` and `budget.manage` are already this word on the wire and need no table entry |
| the gateway, gateway key | earlier prose said "gateway window", "window" and "local key"; the wire kind `openabstractions/local-key@1` and the `INF-W` letter stay |
| hosted server | the upstream vendor behind a hosted model server, for example OpenRouter; the rights resource stays `host:<name>` (row 1 above) |
| `inference/chat.complete` | `abstraction.inference/complete`; the stored-policy dual reader lands with rights, not here |

## Admission

**[INF-A1] Peer binding.** Every call binds the receiving peer by native
Program proof (kernel user and process, bound path). A peer that cannot be
bound, or a peer of another account, reads `forbidden` before the request
is examined further.

**[INF-A2] Evaluation order.** `Start` evaluates in this order and stops at
the first refusal:
1. argument shape and bounds (`invalid`);
2. guarantees and model-server selection through the router (`no_host`,
   reason `router:<verdict>`);
3. the chosen model server's features and required extensions
   (`unsupported_feature`, reason `feature:<name>` or `extension:<key>`);
4. the rights decision for `abstraction.inference/complete` on
   `server:<name>` for the bound subject (`not_permitted`, reason
   `rights:<word>`; `unavailable`, reason `rights:unavailable`, when the
   decision point cannot answer);
5. image content resolution under the original caller's rights (INF-V1);
6. the credential's budget, read from the model server's own declaration
   (`budget_exceeded`, reason `ceiling:<unit>:<credential>`);
7. the caller's concurrent requests (`exhausted`, reason `capacity`);
8. for a hosted server, `abstraction.credentials/applier@1` `Apply` with
   consumer `abstraction.inference/chat@1`, the bound subject, the server's
   credential name and its lowercase host name (`not_permitted` with
   reason `credential:<outcome>:<name>`; `unavailable` with reason
   `credential:unavailable:<name>`).
9. the provider's admission hook, when configured, before accepted execution
   and after the preceding authority and feature checks. Its model activity
   reservation stays visible until the operation is registered or refused.

No byte leaves the service before every step passes. A decision point that
cannot answer never reads as permission. A provider's own refusal — a
resource lease the pool could not satisfy, a lease request the holder
refused, no engine bound, or an unknown model object — reads `unavailable`
with reason family `lease:<outcome>`, `engine:none` or
`object:unknown:<store>/<id>` (DECISION.md D87); this page carries no
refusal word of its own for those causes.

**[INF-A3] Guarantees select model servers.** A request without
`abstraction.inference/hosted-allowed@1` is served only by model servers on
the receiving machine. A request with it may also be served by a hosted
model server whose credential name equals `Request.credential`, ranked
after every resident and would-load local server (router `Pick`). A
request naming both `local-only@1` and `hosted-allowed@1`, or an unknown
guarantee, is `invalid`.

**[INF-A4] Feature gates.** A model server lacking a feature the request
uses refuses it before spend: `tools` needs
`abstraction.inference/tools@1`, `options.json_schema` needs
`abstraction.inference/json-schema@1`, and an `image` part needs
`abstraction.inference/vision@1` and a content reader. A key in
`required_extensions` the model server does not know is refused with
reason `extension:<key>`, and a key listed there but absent from
`extensions` is `invalid`. For a selected native provider or remote runtime,
the receiving runtime reads the operator-managed extension inventory of that
exact registration generation and profile at step 3 of INF-A2. An absent key
refuses before provider activation. A declared key and its value and required
status reach the selected service, which still validates the value and
model-specific support before accepting work. The reserved caller-claim value
is always derived from the bound original subject when forwarding.
A leaf provider over its own OpenAI-compatible image engine declares the
image extensions it carries and maps their values to request fields
(`Config.ImageFields`), such as the Lemonade provider's `lemonade/seed` to
the engine's integer `seed`. Only declared keys reach the mapping. A value it
refuses is `invalid` with reason `extension:<key>`, a field the image request
already sets is `invalid` with reason `extension:fields`, and both end the
call before the engine sees it.
Extension keys under `openabstractions/` are reserved for OA provenance. A
receiving service removes them from the request after validation and before
any adapter, engine or forwarder sees it; vendor keys and their required
status pass unchanged. A request that lists a reserved key in
`required_extensions` is refused with reason `extension:<key>`. The service
records `openabstractions/claimed-program` as `claim` only from a remote
runtime or from a runtime a leaf provider designates; a claim from any other
caller is discarded.

**[INF-A5] Provider-side admission hooks.** A provider whose own program
has work to do on the picked model server before a call is accepted
declares `Config.Admit`, run for the bounded `Start` operations (chat,
transcription, speech, image and live) and `Embed` after
argument, feature, rights, content, output-write, budget, capacity and
credential preflight applicable to that profile, and by the job executor
for every durable job before any call reaches the job's server, on a first
submission and on work recovered after a restart alike. A job whose hook
answers anything but `not_permitted` waits with the hook's word as its
waiting word (an on-demand provider still starting reads
`activating:<name>`) and is asked again at the next sweep; `not_permitted`
fails it with `admission:not_permitted`. The hook is where a declared
on-demand provider starts (facade CONTRACT.md FAC-R8), so it starts for a job
as for a call. The hook receives the
picked server, the model and, through `AdmissionProfile(ctx)`, the
request's profile, so a hook can refuse a model its server declares for
another profile only (`abstraction-router` CONTRACT.md ROUTE-D9). A false answer refuses the request
`unavailable` with the hook's own reason word, or `not_permitted` when
that word is exactly `not_permitted`. A nil hook changes nothing; the
model host declares one to load its object before execution and keep its
own refusal words (`abstraction-provider-modelhost` CONTRACT.md MODELHOST-H2).

**[INF-A6] Admitted work lives only while its rules do.** INF-A2 decides
`abstraction.inference/complete` and the credential's use once, at `Start`.
A running operation of every profile and a running durable job MUST be
decided again, asking nobody, for `complete` on its model server and, when
this runtime applied a credential for it, for
`abstraction.credentials/apply` on that credential (`Config.Standing`;
abstraction-rights RIGHTS-R23):

- before each live `Append` and `Commit`: a refusal ends the session and the
  input reads `closed`; a decision that cannot be obtained sends nothing and
  the input reads `unavailable`;
- before each poll of a durable job's upstream work: a refusal cancels that
  work through the backend's `Cancel` with the plan's headers, which the
  credential audit records when the credential's use was the refusal
  (`Config.CancelledWith`, credentials CRED-A7), and fails the job with the refusal as its reason; a decision that cannot be obtained
  sends nothing, and the job waits as `rights:unavailable` until a sweep
  plans it again (INF-G9);
- on each committed rights change, before the edit returns, and every five
  seconds on the provider's own clock (`Provider.Recheck`): each operation
  a rule refuses is stopped, and a decision that cannot be obtained stops
  nothing there.

A stopped operation ends `cancelled` with the refusal as its reason:
`rights:<word>` for `complete`, `credential:not_permitted` for the
credential. An operation delegated to a remote runtime is decided here on
`complete` for its server, and ending it closes the remote call; that runtime
decides its own credentials. An operation a remote peer started is decided
under the subject its certificate mapped to, so revoking that subject's rule
ends it the same way. The caller's next `Start` is decided, and asked, as a
first one is.

## Image input

**[INF-V1] Digest references.** `vision@1` accepts image parts in user
messages. Each names a canonical lowercase `sha256:` digest and one of
`image/png`, `image/jpeg`, `image/gif` or `image/webp`. The receiving
service reads content under the original bound caller's authority after
the model server's `complete` decision. Storage checks permission before
lookup and while reading. A digest grants no permission, and a grant for
one content action grants nothing under the other: a write grant that
stored the bytes does not authorize reading them back. Request input written
under an input profile is read back under that profile (INF-V4). Authorized missing
content is `invalid` with `content:unknown`; a denied read is `forbidden`
with `content:read:forbidden`; a denied write (the gateway's own storage of
uploaded or generated bytes) is `forbidden` with `content:write:forbidden`;
an unavailable reader is `unavailable` with `content:unavailable`. A denied
write of content a provider generated (`speech@1`, `image@1` or `live@1`
output) is `forbidden` with `output:forbidden`.

**[INF-V2] Verification and bounds.** Before applying credentials or
starting the upstream call, the service verifies the complete bytes
against the digest and checks the detected media type. A mismatch is
`invalid` with `content:digest` or `content:media-type`. The hosted
server's own limits bound each image and the total image payload,
counting every occurrence even when a digest is resolved once. Exceeding
either limit is `invalid` with `content:too-large`. Text and image order
is preserved in the upstream request, including image-only messages.

**[INF-V3] Delegation.** OA calls carry digest references. The upstream
adapter encodes bytes into its vendor format. A remote OA runtime receives
the original references and resolves them in its own authorized store;
delegation does not transfer content. Its admission refusal ends the
delegated request with the existing typed remote refusal.

**[INF-V4] Request input profiles.** Content an application uploads as the
input of a request (an edit input or mask, a vision image, a video's first
frame, audio to transcribe) is written and read under one of two bounded
input profiles: `input:image` for PNG, JPEG, GIF or WebP, and `input:audio`
for the INF-M5 audio types. The application holds a profile with both
`abstraction.storage/content.write` and `abstraction.storage/content.read`
on its resource. The content resolver reads the input back for a request
that same subject starts, under the profile that request consumes:
`input:image` for an image edit, a vision chat or a video job's first
frame, `input:audio` for transcription (`abstraction-storage` STO-W12). The
profile is asked for together with the capability that consumes it: when
a consuming request is refused because its `complete` decision reads
`not_granted` or its input read is refused, the provider tells the runtime
(`Config.AskInput`), which admits the first-use question on
`content.write` for that profile and that proven program. An allow writes
`content.write` and `content.read` on that profile; a never writes both
denials. No upload refusal asks the question by itself. A durable job, an
`image@1` call and a `speech@1` call each decide every grant they need
before they answer: `complete` on the server, the input profile of an edit,
the output profile (INF-S7). Missing several, the request is refused once
with each word in that order separated by a space (for example
`rights:not_granted content:read:forbidden output:forbidden`), and the
first-use question of each is admitted in the same pass, so a person
answers them together; each grant still needs its own answer. Only a
`complete` that reads `not_granted` lets the others be gathered: a denied
`complete` or an unknown action refuses the request with that word alone,
and nothing more is read or asked. A call
refused on `complete` among others reads `not_permitted`, as one refused on
`complete` alone does; a call refused only on its input or output reads
`forbidden`. A durable job refused at submission reads `forbidden` and the
job service seals nothing, so the same key is admitted once the person
answers (`abstraction-job` JOB-A16); a credential the applier refuses for a
reason other than `not_permitted` reads `invalid`. A later check that cannot
answer or finds the request itself wrong reports the grants already refused
instead, since only their answers can let the request run. A decision word
other than `permitted`, `denied`, `not_granted` or `unknown_action` is
`unavailable`. A request that has
not reached its terminal state names its input, and so does an inference
job; the named input stays in the store until then (STO-T7). A job that has
not ended and cannot be decoded names unknown input, which keeps every
object. A refused upload is `forbidden` with `content:write:forbidden`, and
a refused read `content:read:forbidden`, as in INF-V1. Go:
`InputProfileOf`, `InputResource`, `WithInputProfile`, `InputProfileFrom`,
`Provider.NamedInputs`, `JobExecution.NamedInputs`.

## Credentials and spend

**[INF-C1] Applied once per outgoing request.** The service applies a
hosted model server's credential per outgoing request: the first request
of a call and every retry or reconnect call `Apply` again. Applied headers
are sent once and appear in no reply, delta, log record, error or state
file.

**[INF-C2] Budget.** A budget belongs to a credential name: tokens per UTC
day, spend in currency millionths per UTC day, requests per UTC day,
images per UTC day, audio seconds per UTC day, or characters per UTC day,
each independently optional. The provider enforces every unit a budget
record configures above zero, at admission, before an upstream request is
sent; a zero or absent unit enforces nothing. When active declarations share
a credential, the strictest nonzero limit in each unit MUST apply. Removing
or disabling a declaration MUST remove its limits while preserving usage
counts. Requests count one per ended chat call
and one per embed call. `go/ceiling.go`'s `Exceeded` is the measured
behavior: `go/provider_test.go` admits over a tokens budget,
`go/embed_test.go` over a requests budget, `go/image_test.go` over an
images budget, `go/speech_test.go` over a characters budget, and
`go/transcription_test.go` over an audio-seconds budget, each reading
`budget_exceeded` with its own reason unit. The provider counts usage and
cost from provider replies, keeps the counts across its own restart, and
refuses `budget_exceeded` at admission once a count has reached its
configured budget. A request admitted under the budget runs to its end;
its reply counts.

**[INF-C3] One audit record.** One log record per ended call and per
admission refusal: program, model server, model, family, tokens in, tokens
out, wall time, outcome, reason, credential name and budget state. Records
carry names, never header values or message content.

## Modalities

`embed@1` is one bounded call; `transcription@1` accepts one canonical
storage digest and returns ordered, timestamped segment or word deltas
through the shared request lifecycle.

**[INF-M1] Embed admission.** `Embed` admits in `Start`'s order for
modality `embed`, without the feature and capacity steps. The steps are
argument shape, model-server selection, the server's wire, the `complete`
decision on `server:<name>`, the credential's budget, and for a hosted
server `Apply` with consumer `abstraction.inference/embed@1`. No byte
leaves the service before every step passes. A credential scoped only to
`chat@1` reads `not_permitted`, reason `credential:<outcome>:<name>`.
`Embed` reaches an OpenAI-compatible server's embeddings endpoint, a
declared native provider's own `embed@1` over the identity-bound
transport, or a remote runtime's `embed@1`; another wire reads
`unsupported_feature`, reason `wire:<name>`. A native provider receives the
request's vendor extensions and the caller claim of INF-A4.

**[INF-M2] Request and reply shape.** A request carries 1..64 texts of
1..32768 bytes and, optionally, dimensions 1..4096. A completed reply
carries one vector per input in input order, each the base64 of
`dimensions` little-endian float32 values, at most 131072 values in all.
The provider accepts an upstream's number arrays and base64 float32 alike
and returns the same bytes for both. An upstream reply of another count,
length or requested dimension reads `unavailable`.

**[INF-M3] Audit and the gateway route.** Every call leaves one audit
record with modality `embed`, the model server, model, input tokens, wall
time, outcome and reason, and never the texts or vectors. The gateway
serves `POST /v1/embeddings`, returning number arrays or, for
`encoding_format` `base64`, the vectors as sent. A remote server receives
the request unchanged except for the model and the claim extension.

**[INF-M4] Transcription admission.** Admission validates the request,
selects a model server that explicitly serves modality `transcription`,
checks `complete` on `server:<name>` for the original bound caller,
resolves the digest under that same caller's content policy, checks
budgets and capacity, and only then applies a credential and sends bytes
upstream. Unknown content is `invalid`, another caller's content is
`forbidden`, and an unavailable store is `unavailable`. None spends an
upstream request.

**[INF-M5] Input bounds.** Input is at most 16 MiB, its bytes must match
its SHA-256 digest and declared WAV, MP3, MP4, WebM, Ogg or FLAC media
type. whisper.cpp accepts WAV only. References cross OA and remote wires;
bytes exist only in the selected backend request, and a remote runtime
resolves the digest in its own authorized store.

**[INF-M6] Segments and accounting.** Segment text is at most 32768 bytes.
Deltas preserve upstream order and millisecond timestamps; malformed
timestamps or oversized text end `unavailable`. Idle, caller and explicit
cancellation close local or delegated upstream work. Audit and budgets
count the terminal duration in rounded-up audio seconds under modality
`transcription`, an enforced unit under INF-C2.

**[INF-M7] Native and engine transcription.** A model server on the
native transport receives the request with the selected model, no
credential, the local-only guarantee and the claim extension of INF-A4.
Before `Start` the runtime stages the caller's audio, resolved and checked
under INF-M4 and INF-M5, at the provider's `content-writer@1` by its
digest, and relays the provider's segments under INF-M6; a lost `Start`
reply is never retried. A provider that composes an explicit transcription
engine for its own host (`providerhost.NewEngine`) runs the engine in place
of an HTTP adapter: the engine names an unsupported feature before rights,
content and spend (`unsupported_feature`, its reason), and each segment it
emits is checked as an upstream's.

## Preload

The first request of a conversation on a local model waits for its whole
prompt to be evaluated. OpenCode's Local assistant's first answer on
Qwen3.6-35B-A3B took 10.6-12 s with its about 3,200-token request, and 3.6 s
with the same request's fixed prefix evaluated ahead
(`research/live-runs/2026-10-02-demo-model-opencode.md`, "Results" and
"Pre-warm"). `preload@1` lets an application have its own prefix evaluated
before it sends that request.

**[INF-PL1] The request is the caller's own prefix.** `Preload` takes a
`chat@1` `Request` whose last message is not a user message: the caller's
next request is these messages and tools followed by its user message. A
last user message reads `invalid`, reason `messages:last_user`; an image
part reads `invalid`, reason `part:image`. The provider and the model
server keep nothing of the request after the call, and nothing one caller
preloaded is evaluated for another.

**[INF-PL2] Admission is Start's, and only a prefix-evaluating server takes
one.** `Preload` admits in `Start`'s order: argument shape, guarantees and
model-server selection for modality `chat`, server support, the `complete`
decision on `server:<name>` for the original bound caller, then the
provider's model load (INF-A5). A server takes a preload when the provider
sends to it itself and composes a prefix evaluator for it
(`Config.EvaluatePrefix`), or when it is a declared native provider whose
declaration lists `abstraction.inference/preload@1` beside `chat@1`
(`router.Host.Preloads`); that provider receives the request with the
selected model, its vendor extensions and the caller claim of INF-A4. A
hosted server, a remote runtime and every other server read
`unsupported_feature`, reason `preload`. The evaluator receives the engine
request the chat path makes of the prefix (`openAIBody`), so the server
renders the bytes the following `chat@1` request renders.

A preload is speculative work and holds the lowest standing, the
coordinator's decision of 2026-10-03 under the open arbitration question
of VISION 2026-09-06 ("automatic prioritisation of one process over
another ... is arbitration"), which it settles for nothing else. The
admission marks itself (`PreloadAdmission`), and a provider's `Admit` loads
for it only without asking another holder to yield; otherwise it answers
`busy`, reason `resources:held` (`ReasonResourcesHeld`), and loads nothing.
A preload reserves no model activity for its load or its evaluation, so no
idle clock or yield waits for it. A provider on the resource service's
leases loads through the holder's speculative ensure (resource RES-L6).

**[INF-PL3] The reply says what the server did.** `evaluated` carries the
tokens evaluated; `loaded` means the model is loaded and nothing was
evaluated, because the prefix is shorter than the provider's minimum; `busy`
means other work comes first and nothing was evaluated: the server served
another request since it loaded or during the evaluation (`engine:used`),
another preload runs there (`preload:running`), or loading the model would
ask another holder to yield (`resources:held`); `refused`
carries the server's refusal of the prefix. Every refusal carries the
reason words of `Start`. The call returns when the server has evaluated the
prefix, refused it, or the service's call budget of 35 s ends; a load
longer than that budget reads `unavailable`, reason `caller`, and leaves the
model loaded under its lease.

**[INF-PL4] What the shared server shows.** A preload widens nothing: a
caller can have only its own text evaluated, and a busy server evaluates
nothing. The limit that exists without it remains: a model server's prompt
cache is one per server, and a caller can read from its own request's
`prompt_tokens_details.cached_tokens` how many leading tokens of its prompt
the server already held, whoever sent them. Every audit record of a preload
has modality `preload`, the model server, model, the tokens evaluated,
wall time, outcome and reason, and never the messages or tools.

## Requests

**[INF-Q1] Identity and visibility.** A request is identified by its
opaque `operation` id within the provider epoch, and is visible only to
the account and program that started it. Any other caller reads `unknown`
for `Observe` and `Cancel`.

**[INF-Q2] Delta paging.** Deltas are numbered from 0 without gaps, and the
last is `end`. `Observe` returns deltas from `cursor`; the newest
`Admission.retained_deltas` deltas are readable, and an older cursor reads
`gap` with `next` at the oldest retained sequence. `max_bytes` bounds the
compact JSON encoding of the returned deltas. `at_end` is true when the
page holds the end delta or `cursor` is already past it.

**[INF-Q3] Idle cancellation.** A request with no `Observe` in flight and
none received for `Admission.idle_ms` is cancelled with reason `idle`, and
its upstream request is closed within one second after that. A caller
that exits therefore stops spend within `idle_ms` plus one second. A
caller that disconnects in the middle of an `Observe` stops that wait at
once.

**[INF-Q4] Retention.** An ended request stays observable for
`Admission.retention_ms`, then reads `unknown`. `Cancel` of an ended
request reads `ended` with its reply outcome and changes nothing.

**[INF-Q5] No request identity for Start.** A retry of `Start` is a new
request. The contract offers no idempotency key for `Start`: a lost
`Admission` leaves a request that idles out within `idle_ms` unobserved,
at most one request's spend.

**[INF-Q6] Clients.** Each language's client offers `complete()`, which
starts a request, observes it to its end and folds the deltas into a
`Reply`, and `stream()`, which yields deltas in the language's native
iteration form and cancels the request when the iteration is abandoned. A
`Start` refusal is a `Reply` with that outcome and no deltas. Clients
never retry `Start`.

**[INF-Q7] Context overflow.** A model server that refuses a request as
larger than its context window ends it `refused` with reason
`upstream:context_length_exceeded:<window>:<input>`, the word OpenAI,
Lemonade and OpenCode already use. This applies to an HTTP error answer
and to an error event inside a stream. The server's error code
(`context_length_exceeded`, `exceed_context_size_error`,
`model_context_window_exceeded`) or its overflow wording identifies the
refusal; a rate or quota message never does, and a 429 keeps its status
reason. `<window>` is the per-request context in tokens the server stated
with the refusal. When it stated none, `<window>` is the router's latest
effective-context observation (router `Alias.effective_context_length`) for
the model server and model that refused. `<input>` is the counted input
tokens the server stated. Each is a decimal and 0 when unknown. The reply,
its deltas and the audit record carry these two numbers only: the upstream
body, message, prompt text and headers are discarded. Parts already
delivered stay delivered. An engine that exits or closes the stream without
a terminal event remains `unavailable` with `upstream:stream`, a stream
error of another kind remains `unavailable` with `upstream:error`, and an
unreachable server remains `upstream:unreachable`.

## Speech

**[INF-S6] Speech delivery.** `speech@1` streams bounded audio chunks and
returns its complete bytes through a typed `Delivery`, using the shared
request lifecycle and original-caller authorization.

**[INF-S7] Output authorization.** Generated output (`speech@1`, `live@1`,
`image@1`, `image@2`, batch image work and a video job's clip) requires both
`abstraction.storage/content.write` and `abstraction.storage/content.read`
on its exact `output:speech`, `output:live`, `output:image` or `output:video`
resource before paid work; a video job writes under `output:video` and an
image batch under `output:image` (`OutputVideo`, `OutputImage`). A
synchronous call or a job refused for want of its output profile admits
the paired first-use question on `content.write` for `output:image`,
`output:speech` or `output:video` and that proven program, as INF-V4 does
for input. The runtime admits it where it authorizes the output write
(`Config.PrepareContentWrite`); a host whose preparer does not ask is told
of a refused job through `Config.AskOutput`. The call or job is refused at
once and succeeds on retry after an allow. An allow writes both rights on
that profile, a never both denials. `output:live` pairs nothing. The read it grants reaches only results that
same subject's own work produced, because a result is read back only by the
subject its provenance names (below). The runtime binds this to the original account and proven program,
media type and byte limit, and rechecks both rights at publication. After a
digest-verified commit, the runtime durably records that subject, digest and
profile before returning the result. A generated result is readable only by
that subject while its profile read right remains permitted; an explicit
digest read denial or policy outage cannot be overridden by the profile.
The reader rechecks on each access. Digest-addressed uploads keep their
existing digest policy and gain no read right from writing. Once result
publication starts, cancellation waits for that publication's outcome.

**[INF-S8] Native and engine speech.** A model server on the native
transport receives the request as INF-M7 states and decides its own
formats; the runtime relays its audio chunks as they arrive, reads the
provider-local delivery from the provider's `content-reader@1`, checks it
against the relayed bytes' digest, size and media type, and publishes it
under INF-S7. A provider that composes an explicit speech engine for its own
host runs it in place of an HTTP adapter: the engine names an unsupported
feature (a format or voice) before output authorization and spend, and its
complete audio must be the requested format by content and within the
delivery bound before it is streamed and published.

## Live

**[INF-L1] Live delivery.** `live@1` accepts ordered PCM input, streams
transcript and audio deltas, and commits one final response, using the
shared request lifecycle and original-caller authorization (INF-S7).

**[INF-L2] Input framing.** `Append` uses mono signed 16-bit little-endian
PCM at 24000 Hz, in even-sized frames of 1..65536 bytes. Retrying the most
recently acknowledged identical frame returns `duplicate` and sends no
bytes upstream. Other repeated or skipped sequences return `out_of_order`.
An uncertain send terminates the session; accounting includes the
attempted frame.

**[INF-L3] Commit.** `Commit` needs at least 100 ms of input (4800 bytes),
closes input and requests one final response. Repeated `Commit` is
idempotent. `Observe` retains its cursor/replay semantics after `Commit`.

**[INF-L4] Wire selection.** The OpenAI Realtime wire is explicitly
selected; ordinary OpenAI-compatible model servers do not acquire live
support automatically. The native service and Go client support live
sessions. Service-to-service forwarding uses the authenticated remote
frame transport and attributes final deliveries to the remote trust
domain; remote credentials, output authorization and accounting remain
owned by that runtime.

**[INF-L5] The gateway route.** The optional `/v1/realtime` WebSocket
route supports one turn, under INF-W8. Unsupported session options and
events are refused. The gateway key and peer authority are rechecked
during the session. Loopback protocol fixtures prove this event mapping.

## Model-server registration

Every model server the runtime reaches is a declaration of role `host` in
`abstraction.facade/registry@1` (facade CONTRACT.md FAC-R6, FAC-R7; D10: the
registry's own role value stays `host` on the wire, and prose reads it as a
model server). Three sources declare one: the operator through `AddHost`, a
product from its own record of where it listens, and the installation from a
declaration file beside the runtime executable. `hosts.json` keeps the
hosted server's own bounds and the `declared` switch; its entries became
declarations at the first start on this build (facade CONTRACT.md FAC-R1). A
model server's budget lives in its own declaration alone, never duplicated
in `hosts.json` (inference-facade review finding 2). Provider, inventory-
source and remote-runtime declarations are governed by the facade registry
(facade CONTRACT.md FAC-R1..FAC-R4, FAC-R8), not by this page; delegation to a
remote runtime stays here, under "Remote runtimes".

**[INF-H1] Product declarations.** A product declaration is read from the
product's own record of where it listens, and nothing else. A product
that recorded nothing declares no model server: its documented default
address is an installation declaration instead. A malformed record is no
declaration, with a logged reason. No probe opens a connection, scans a
port or listens for multicast. A product's declaration lists
`declared_by` as the product's name and shadows the installation's
declaration of that name; the installation's lists `installation`.
`hosts.json`'s `declared` switch turns the product probes off; removing a
product's or the installation's model server through `RemoveHost` disables
that name (facade CONTRACT.md FAC-R7) and leaves the other model servers as
they are. [README.md](README.md) names each supported product's own record
and documented default address (inference-facade review finding 11: this
rule states the promise, not the product table).

**[INF-H2] Modalities per server.** Every model server carries modalities:
those its declaration's `profile:<name>` resources name, or its wire's
default. A local product's models carry their own modalities from its
metadata where it reports them: LM Studio `type` `llm` and `vlm` serve
`chat` and `embeddings` serves `embed`; Ollama `/api/show` capabilities
`completion` serves `chat`, `embedding` `embed` and `image` `image`.
router@1 `Pick` takes the modality beside the model and picks only a name
its server serves for it; with no servable server serving the modality, or
only servers that serve the model for other modalities, the verdict is
`no-host`. chat@1 picks with modality `chat` and refuses such a request
`no_host` with reason `router:no-host`.

**[INF-H3] Hosts, AddHost and RemoveHost.** `Hosts`, `AddHost` and
`RemoveHost` are fronts over the registry's declarations of role `host`,
and keep their own action, `host.manage` on resource `account`. `Hosts`
reads every enabled model-server declaration in name order, with the
router's survey and each hosted credential's spend today; a declaration
an operator disabled is not a model server and is not listed. `AddHost`
writes one declaration of role `host` declared by `operator`, with the
entry's modalities or its wire's default, and its budget in the
declaration alone. `RemoveHost` withdraws one: the operator's own
declaration is deleted, and a product's or the installation's is disabled
by name (facade CONTRACT.md FAC-R7), which answers `applied` with reason
`disabled`. Both are conditional on the registry's revision, which
`Hosts` reports, and a name no enabled declaration holds reads `unknown`.
A declaration of any other role leaves through the registry's own
`Withdraw`. `Hosts` only reads, and INF-P2 names the edit that writes a
rights rule.

## Gateway keys

**[INF-K2] Minting.** `IssueKey` mints 32 random bytes, holds them as a
credential-manager record of kind `openabstractions/local-key@1`, and
returns the key once. The credential manager never applies that kind to
an outgoing request, and `Store` refuses it. A program holds at most one
active key. The gateway decides a key's consumer per request (INF-W5,
inference-facade review finding 8).

## The gateway

The gateway is the runtime's loopback surface for programs that only know a
base URL and an API key: OpenAI `POST /v1/chat/completions` and `GET
/v1/models`, and Anthropic `POST /v1/messages`, with server-sent events on
both. It is the same provider, rights decision and audit as the native
route.

**[INF-W1] Listening.** The gateway listens on IPv4 loopback only, and it
is closed by default. It opens from the gateway setting in the runtime
state (`inference/gateway.json`), which operator@1 `SetGateway` writes and
applies to the running runtime, or for one foreground runtime from
`--gateway 127.0.0.1:<port>`, which leaves the setting unchanged. Closing
it stops the listener and every connection before `SetGateway` replies,
and a runtime restart reopens it from the setting.

**[INF-W2] Peer binding.** Each accepted connection's peer is bound
through the socket-owner table (identity `BindLoopback`, ID-T1..T6) before
a byte is read, and must meet user, process and path at `bound` as an
account of the runtime. The binding runs in the connection's own goroutine
on its first read; one program's verification holds no other connection. A
peer that cannot be bound is answered 403 and the connection closed with
its request unread. On macOS every connection is refused this way. The
binding verifies the peer's code signature as the native pipe binding
does, and each rights decision for the connection carries that peer (asks
ASK-A6). A process's first connection pays one verification, and its later
connections reuse the verdict for five minutes (identity ID-W2). A
keep-alive connection whose verdict is older than five minutes is bound
and verified again before its next request is decided, and must answer for
the same program.

**[INF-W3] Rechecked binding.** A request's binding is rechecked after its
head is read and again after its body, before admission. A connection
that no longer answers for the program that opened it is refused with
reason `binding:peer_moved`.

**[INF-W4] Key verification.** The gateway key (`Authorization: Bearer` or
`x-api-key`) is verified against the credential-manager record of kind
`openabstractions/local-key@1` in constant time before the body is read. A
missing, unknown or revoked key is 401; a key issued to another program
than the bound one is 403 with reason `key:wrong_program`. Refusals set
`Connection: close` and read no body.

**[INF-W5] What a key admits.** A key grants nothing by itself. A request
is admitted through `Start`, `Embed` or a live session for the bound
subject. `abstraction.inference/complete` on the model server and, for a
hosted server, `abstraction.credentials/apply` decide it — with consumer
`abstraction.inference/chat@1` for `/v1/chat/completions` and
`/v1/messages`, `embed@1` for `POST /v1/embeddings`, and `live@1` for `GET
/v1/realtime` (inference-facade review finding 8). A key issued with a
credential name adds `hosted-allowed@1` and that name to every request; a
key without one adds `local-only@1`.

**[INF-W6] Audit route and rung.** Every gateway refusal before admission,
and every admission refusal and ended request of the gateway, is one
audit entry with route `window` and the rung of the bound peer
(`tcp-loopback/<platform> user=<proof> process=<proof> path=<proof>`, or
`... unbound`). Native entries carry route `native` and the pipe or socket
rung.

**[INF-W7] Status mapping.** `Start` refusals map to HTTP statuses:
`invalid` and `unsupported_feature` 400, `not_permitted` and `forbidden`
403, `no_host` 404, `budget_exceeded` and `exhausted` 429, `refused` 502,
others 503. The error body carries `type`, and `error` with `type`,
`message` and `code` (the outcome word), readable by both wires. A
`refused` reply whose reason is a context overflow (INF-Q7) is the client's
invalid request: status 400, `error.type` `invalid_request_error` and
`code` `context_length_exceeded`. Its message names the window and the
input when each is known and carries nothing else from the reason. A reply
that ends other than `completed` in a stream is an error event, then the
stream's end. A request field chat@1 cannot express (`n` other than 1, an
image part, an unknown response format) is `unsupported_feature` before
admission. A client that disconnects cancels its request.

**[INF-W8] Realtime.** `GET /v1/realtime` is the gateway's Realtime
WebSocket, one `live@1` session per connection. The connection URL names
the model (`/v1/realtime?model=<name>`), a connection that names none is
`invalid`, and `abstraction.inference/complete` on the model server that
model routes to is decided for the bound program before the upgrade
completes, answered with INF-W7's status on an unupgraded connection.
`session.update` selects PCM16 at 24000 Hz with manual turn detection and
audio output, and carries the voice; it may repeat the URL's model and may
not name another one. `input_audio_buffer.append` carries one base64
PCM16 chunk of 1..65536 even bytes to `Append`;
`input_audio_buffer.commit` closes input; `response.create` invokes
native `Commit` and begins the turn. `response.output_audio.delta` and
`response.output_audio_transcript.delta` carry the session's deltas, and
`response.done` the terminal outcome and usage. Every refusal after the
upgrade is one `error` event naming the typed outcome, then the close.
The wait budget is INF-Q3's `idle_ms`, reported by the pre-upgrade
decision: a connection that sends no client event for that long is closed
and its session cancelled. Disconnect, a lost binding, a revoked key and
gateway shutdown each cancel the session.

**[INF-W9] Gateway administration.** `Gateway` and `SetGateway` are
decided on `host.manage` on resource `account`. `SetGateway` is
conditional on the setting revision, the digest of the setting file, and
refuses an address other than `127.0.0.1:<port>` as `invalid` with reason
`address`. A setting written whose gateway cannot listen reads
`unavailable` with reason `listen:<detail>`, and `Gateway` reports the
same reason in `why`.

## Operator

**[INF-P1] Rights actions.** `Hosts`, `AddHost` and `RemoveHost` are
decided on `abstraction.inference/host.manage`, `Keys`, `IssueKey` and
`RevokeKey` on `key.issue`, and `Audit` on `audit.read`, each on resource
`account` for the bound caller. The default installation grants them to
the runtime's operator programs.

**[INF-P2] Model-server edits and their rights side effects.** Edits to
model-server declarations are conditional on the registry's revision
(INF-H3). `AddHost` writes the permit rule `complete` on `server:<name>`
for the operator programs and the caller, and for a hosted server with a
credential the runtime program's `abstraction.credentials/apply` on
`credential:<name>`, with why `inference server add`; an existing rule on a
target is left as it is. A hosted server's own survey runs as the
runtime's operator program, the identity `abstraction.router/router@1`
applies every hosted credential under. That pairing's `apply` rule for
the runtime program is written by whichever edit creates it — `AddHost`
when the credential is already registered, or the credential's own
`Store` when it names an already-declared model server — and `Hosts`
only reads that state, naming the program a missing rule belongs to in
its `why` (inference-facade review finding 7, against the former
contradiction with INF-H3). The router reads the new model servers at
once. The stored server records its modalities (the wire's default when
the request names none: `router.DefaultProfiles`' table for the server's
wire, read through the same construction the router routes through —
every seeded modality for `openai-compatible` and the local kinds, `live`
for `openai-realtime`, `transcription` for `deepgram-prerecorded`,
`speech` for `elevenlabs-stream`, `image` for `stability-v2beta`,
`fal-queue` and `replicate-predictions`, `chat` for `anthropic-messages`
and an owned `<owner>/<name>@<n>` kind) and `declared_by` `operator`; a
request that names `declared_by` is `invalid`. The router picks by model
and modality (INF-H2).

**[INF-P4] Audit journal.** The audit journal keeps the newest 4096
entries across runtime restarts, numbered without gaps. Entries carry
names and counts, never a header value, key or message content.

## Resource actions

**[INF-R1] Closed catalogue.** `resource_actions` is closed: `complete` on
`server:<name>`, and `host.manage`, `key.issue`, `audit.read` and
`selection.manage` and `selection.manage_for` on `account`. The default installation grants none of
them to applications. `selection-operator@2` manages policies for the bound
account and OS-observed program. Chat@2 resolves that same scope
for its bound caller and subjects every selected candidate to the existing
`complete` decision and admission gates. Future actions require an explicit
catalogue revision. `selection-operator@2` uses that self scope when
`target_program` is empty or names the bound program. In that self scope
`Read` needs no grant: a program reads its own policies, the ones chat@2 and
image@2 already apply for it. `Put` and `DeletePolicy` keep
`selection.manage`. Another canonical
absolute executable path requires `selection.manage_for` on the bound
account, for `Read` as for the edits. Installation grants that action only to configured operator programs.
The target is a policy namespace; it never supplies the requesting identity.
The service resolves policies under the observed caller program, including for
applications whose program was configured before launch or registration.

Version 2 policies add an optional logical provider declaration name to each
candidate. The service requires that declaration to exist in the current
eligible host generation and satisfy the requested profile and placement.
A missing or stale pin refuses selection without trying a different provider.
The model remains a router family or alias. Adapter installation configuration
and provenance own exact binary and model artifact hashes. Pinned and unpinned
policies retain the same stored JSON format and revision checks. The unpublished
`selection-operator@1` profile is retired; current discovery and clients use
`selection-operator@2`.

For a named policy target, `RequestContent.guarantees` and `credential` are
empty. The stored candidate supplies local-only or hosted-allowed intent and,
for hosted placement, the credential name. The service derives one concrete
legacy request for that candidate and independently checks current host rights,
credential application, budget and capacity. Nonempty inline choices in policy
mode are invalid. An explicit model target retains chat@1 guarantee and
credential behavior.

`image@2` uses the same target and scoped policy with image request content.
Its explicit model calls retain image@1 validation and admission. A policy
target leaves inline guarantees and credential empty. Each candidate is
filtered for the image profile before the image@1 admission path. Selection
advances only when a route is proven unavailable before admission. An
ambiguous or attempted provider start, rights refusal, feature refusal,
credential refusal or budget refusal ends that call without fallback.

## Remote runtimes

`chat@1` names no base URL, header, key, endpoint or local path. A request
carries a credential *name*, the request id is opaque, and `Reply.host` is a
display name. A local runtime can therefore delegate a request unchanged to
another runtime holding the credential through the mutual-TLS remote
transport: the remote binds its own client certificate subject, applies the
name from its own credential manager under its own rights, and the local
service relays deltas and records the delegation as an attributed claim. No
applied header or local pipe proof crosses the network.

A remote runtime is declared in `abstraction.facade/registry@1` (facade
FAC-R4). A chat@1 request the router picks for it is delegated: the request
goes unchanged except for the chosen model name and the extension
`openabstractions/claimed-program` set to the bound program, its deltas are
relayed, and cancelling it cancels the remote request. Local budgets and the
local applier are not used. The receiving runtime maps the client
certificate key to its caller, decides and applies for the subject {mapped
namespace, its own executable}, and records route `remote`, `domain` the
namespace and `claim` the claimed program. The delegating runtime records
`domain` the remote host. The receiving runtime answers endpoint@1
`Describe` over the remote transport with chat@1 and router@1.

The gateway is a loopback surface of one machine and never a remote one: a
peer on another machine has no socket-owner record here, and INF-W1 keeps
the listener on 127.0.0.1. A remote operator reaches operator@1 the way it
reaches chat@1, through the remote transport, where the receiving runtime
maps the client certificate to its own subject and decides the operator
actions for it. A gateway key minted on one runtime means nothing to
another.

## Engine files

A provider that launches an engine it did not build, such as TheNoise or
Lemonade and its backends, proves the engine's files at launch and holds them
unchanged while the engine runs (VISION.md, 2026-10-01). The shared provider
kit `go/providerhost/enginefiles` implements these rules once, and every such
provider uses it.

**[INF-F1] Full hash.** The provider hashes every file of the engine tree
against its pinned manifest at install, at the first launch of each provider
process, and at the launch after any refusal. It refuses a tree that holds a
file or folder the manifest does not list, lacks a listed file, holds a link
or other special entry, or holds a file whose size or SHA-256 differs. A
file or folder the provider cannot open, read or identify, such as one
another program holds open for writing, refuses the launch as unreadable
and keeps the system error as its cause. The kit names the two refusals
apart (`ErrMismatch`, `ErrUnreadable`): a changed tree needs a new install,
and an unreadable one launches once the file is free. An entry that no
longer exists is missing.

**[INF-F2] Metadata check.** Every other launch compares each file's ID,
size, last-write time and change time with a record written after the last
full hash. The record is bound to the manifest pin, the tree's final path and
its volume, and lives in the provider's private state. The provider process
keeps the SHA-256 of the record it wrote. A record it did not write, or any
extra, missing or changed entry, sends the launch to the full hash. A
metadata launch trusts the account's own processes not to have set file
timestamps back: SetFileTime restores a last-write time, and
SetFileInformationByHandle restores a change time as well. The full hash at
the first launch of each provider process is the guarantee against such a
writer.

**[INF-F3] Read-only folders.** At install the provider gives every folder
of the tree a protected DACL: the account may read, execute and list; SYSTEM
and Administrators have full control. Each launch checks every folder's DACL
and refuses a folder with any other. Setup installs a replaced tree again,
and the kit's `Unlock` returns full control of the folders to the account
for replacement or removal. A remover of a folder that may hold installed
trees gives them back first through the kit's `UnlockWithin`, and an
uninstaller runs the provider's `engine unlock` before the provider's
program goes.

**[INF-F4] Held files.** A launch opens every file and folder of the tree
with read sharing only before it checks them. The provider keeps them open
until the exit of the engine's process tree is confirmed; an unconfirmed exit
keeps them open. Windows refuses writes, renames and deletes of held files
during the run. A killed provider's handles close with its process, and its
successor's first launch hashes in full (INF-F1). The first live run of each
engine confirms that it opens every tree file with read sharing.

**[INF-F5] What is guaranteed.** On Windows the tree equals the manifest when
the launch starts, and no file changes while the engine runs. The account
owns the folders and can change their DACL. The DACL stops ordinary software
and accidents between runs; the held handles stop a swap during the run.
Between the first launch of a provider process and a later one, the guarantee
rests on the metadata check, which trusts the account's own processes not to
have changed a file and set its timestamps back (INF-F2).
Outside Windows the kit runs the same full hash and metadata check and keeps
the files open. There the open descriptors block no write, rename or delete,
and the kit sets and checks no folder ACL.

**[INF-F6] Install from a pinned archive.** A provider that stages an engine
tree from a publisher's archive, a ZIP or a gzip-compressed tar, uses the
kit's `InstallFromArchive`. A tar entry's leading `./` is dropped, and a tar
link, device or any entry other than a folder or regular file is refused. The
archive is held with read sharing only (on Windows) and its size and SHA-256
must equal the pin before any entry is read. Each kept entry is chosen by
its pinned name, created with `O_EXCL` in a stage folder beside the tree,
read through a reader bounded by its pinned size, and refused unless its
size and SHA-256 equal the pin; every other file entry must be one the
review excludes, and is never read. An entry name that is not a plain
relative path, or names a Windows device, refuses the install, as does a
second entry of the same name in any letter case. A provider whose pin is
the digest of a manifest recorded from the archive (TheNoise) takes every
file entry and checks that manifest before anything is published; such an
install replaces no folder. An existing folder that proves against the
manifest is kept. One that differs from it is refused unless the caller
asks for replacement; one with an unreadable file (INF-F1) is refused. A
folder a running engine holds (INF-F4) is refused before its ACL changes.
Otherwise the stage replaces it, the old folder waits as
`.previous` until the new tree is installed, and it returns with its
read-only folder ACL when a rename or the install fails. A `.previous` left
by a killed replacement refuses the next install. The new tree is then
installed (INF-F1, INF-F3). One Verifier runs one install of a tree folder
at a time. The folder is keyed as the filesystem names it (VISION.md L623):
the part of its path that exists by its final path, with links, junctions
and 8.3 aliases resolved by the volume, and the folders still to be created
below it by name, compared as the file system compares names
(`abstraction-identity` `PlaceKey`), so the key is the same before and after
the install creates the folder. No case is folded off the volume's rule.

**[INF-F7] Files proved where they lie.** A provider that runs an engine on
model files kept in a store OA does not own, a person's model folder or
another application's, proves them in place with the kit's `LaunchPlaced`
and `CheckPlaced` and copies none (VISION.md, 2026-10-01). Each file is named
by absolute path, size and SHA-256; a relative path, a file named twice and a
digest that is not lower-case hex are refused before anything is opened.
Each file is opened with read sharing only, its size checked, and then
proved as a tree file is: by its full SHA-256 at the first proof in the
process and after any change (INF-F1), and by its ID, size, last-write and
change time against the provider's record otherwise (INF-F2). The files stay
open until the Hold is released, which a provider does when the engine's
exit is confirmed (INF-F4). No folder is listed and no ACL is set: other
files beside a placed one are the store's.
Every folder on a placed file's path, from the volume's root down, is opened
before the next is resolved through it, refused when it is a reparse point
(a junction, symbolic link or mount point), and held without delete sharing
until the Hold is released, so it cannot be renamed, deleted or replaced by
a junction. `Hold.Paths` is the final path of each held file
(`GetFinalPathNameByHandleW` on Windows), and a provider passes the engine
those paths alone. `CheckPlainPath` refuses a path through a link the same
way, for a provider choosing among candidate files.
`HoldDirectory` retains this same namespace lifetime for a mutable control
folder, while its caller separately proves the folder and record authority.
The metadata shortcut applies to a placed file only when it passes the
trusted-in-place check for the owners the provider names with it
(`trustedpath.CheckFile`, identity ID-D2): a trusted owner owns the file and
every folder above it, no other principal can change or remove the file or
delete, rename or re-permission what those folders hold, and no reparse point
is on the path. The check runs while the path is held. A file that fails it,
or has no owners named, is hashed in full at every launch; `Hold.Hashed`
counts the files a launch read.

## Model memory

A provider host places a model's bytes in memory once and lends them,
read-only, to each engine it launches (VISION.md, 2026-10-01;
`research/model-locators/MEMORY-LENDING.md`). The shared provider kit
`go/providerhost/modelfiles` implements these rules once; it lives beside
`enginefiles` because the provider host is the process that creates engine
processes. The section is provider-side data: it never reaches an
application or a contract field, and it never crosses machines.

**[INF-MS1] Fill without a file.** `Fill` creates an unnamed,
pagefile-backed section the provider host owns and places the object's
bytes in it from one source: a whole file, a STORED region of a container,
or one DEFLATE stream, decoded with Go's `compress/flate` on one core. A
stored source is read with positioned reads on at most eight handles,
unbuffered when its offset is a multiple of 4,096. No source is extracted,
and the fill creates no file, temporary or otherwise. The whole size is
charged to commit once, at creation.

**[INF-MS2] Proof.** `Fill` takes the object's digest, `sha256:` and 64
lower-case hex digits, the digest of its uncompressed bytes that storage
reports. It hashes every byte it places: the whole object, and each
16 MiB chunk for later checks. A DEFLATE stream must give exactly the
object's length and match the member's CRC-32. Bytes whose digest differs
are `ErrDigest`; a source that cannot give them is `ErrSource`; a cancelled
fill returns the context's error. Each of these frees the section before
`Fill` returns. A digest that does not decode as 32 bytes of lower-case hex
is refused before a section exists.

**[INF-MS3] Read-only.** After the proof the provider host unmaps its
writable view and replaces its own creating handle with a duplicate holding
`SECTION_MAP_READ` and `SECTION_QUERY` only, closing the source. From then
on no writable view of the section exists and no handle in any process can
map one. The section's descriptor is the protected DACL `D:P(A;;0x5;;;OW)`:
the owner may query and map for reading, and the OWNER RIGHTS entry takes
away the owner's implicit `READ_CONTROL` and `WRITE_DAC`. Neither the
provider host nor an engine of the same account holding its loan can map
the section for writing, raise a view to read-write, or duplicate a handle
to map-write or `WRITE_DAC`. The proof made at the fill holds for the
section's lifetime. Every loan is read-only (VISION 2026-10-01); no
writable loan exists. The account stays the boundary, as for files: an
engine can copy its read-only handle into another process of the same
account, and a process holding `PROCESS_DUP_HANDLE` on the provider host
can duplicate the host's read-only handle.

**[INF-MS4] Loan.** `Lend` gives one engine an inheritable duplicate of the
section with `SECTION_MAP_READ` and `SECTION_QUERY` only. The engine
receives it at launch through supervision's handle list (abstraction-facade
CONTRACT.md FAC-P1), with the handle value and length on its command line,
and the provider host closes its copy of the duplicate once the engine has
started. The loan is a lease that ends when the engine's process tree has
exited, as supervision confirms it, or when the engine reports that it
released the section. A loan holds the section until it ends: a caller
whose engine does not start ends the loan at once with `End`.

**[INF-MS5] Free.** Sections have no warm-residency rule: a section is
freed when its last loan ends, or when its owner closes it with no loan
open, and lends nothing afterwards. The end that frees the section closes
it to new loans in the same step, so a `Lend` racing that end is refused
with `ErrReleased`. The caller passes `Fill` the release of
the `abstraction.resource` holding it took for the section's memory, and the
kit calls it exactly once: when the section is freed, or when `Fill`
refuses.

**[INF-MS6] Platforms and engines.** The kit is built on Windows. On Linux
the design is a sealed `memfd` (`F_SEAL_SHRINK`, `F_SEAL_GROW`,
`F_SEAL_WRITE`, `F_SEAL_SEAL`) that an engine maps, or opens as
`/proc/self/fd/N`; it is not built, and `Fill` there answers
`ErrUnsupported`. No engine OA launches reads a model from an inherited
handle at its pinned version, so the model host declares `file` only
(abstraction-provider-modelhost CONTRACT.md MODELHOST-L1), and no launch
lends a section yet.

**[INF-MS7] Span record.** OA's own record of an object is its `sha256:`
digest, its length and the SHA-256 of each `ChunkSize` span of its bytes,
made only when OA hashed every byte: by a fill that hashed the whole object
(INF-MS2, INF-MS8) and by the packer, which hashes the object it packs
(INF-MP6). The kit's `SpanRecords` keeps one record per digest in a folder
of the provider host's private state, bound to the span size, and is the
one place OA reads span digests from before it trusts them (INF-MP1). A
record that does not parse, names another digest or span size, or holds the
wrong number of spans is no record: the next fill hashes the whole object.
A record that could not be written leaves no record, and the fill that
made it reports the failure on its section (`Section.RecordErr`, INF-MS8).
A record is written to a `.record-` file in the folder and renamed into
place; opening the folder removes such files a killed writer left. The
folder is private to this account through OA's one private-folder helper,
abstraction-identity's `go/privatedir` (identity ID-D1), the same one the
engine socket root uses (INF-EL2): created with a protected DACL that allows this
account and SYSTEM, set at CreateDirectory, on Windows and mode 0700
elsewhere; a folder that is a file or a link, that another principal owns,
or whose DACL is inherited or admits anyone else (any group or other bit,
elsewhere) is refused (`privatedir.ErrNotPrivate`). The account stays the
boundary, as for the section itself (INF-MS3). The record lives with the fill and
the span size, beside the engine-file records the same host keeps (INF-F2),
and never crosses machines; `abstraction.storage` keeps the object's
identity (MEMORY-LENDING.md §2, §3).

**[INF-MS8] Fill through a decoder.** `FillDecoded` creates a section as
`Fill` does and lets a decoder place the object's bytes in its writable view
from a STORED region of a file, with no file written. It needs
`SpanRecords`. The fill owns the proof: after the decoder returns, it
hashes every span itself. When `SpanRecords` holds the object's record, the
spans must equal the record's, and the decoder is given them and may refuse
early. Otherwise the fill hashes the whole object against its digest as
well and records the spans (INF-MS7). A decoder may leave bytes for the
hash pass to complete: it returns a function the fill calls once per span,
on several goroutines at once, and the function that call returns gets the
span's bytes in order in blocks of 256 KiB, each just before the fill
hashes it, and completes them in place without writing another span's
bytes. The hash then reads each block once while it is in cache. After the proof the writable view is dropped and the section
sealed (INF-MS3). A record of another length, bytes of another digest, a
decoder's error and a cancelled fill free the section, as INF-MS2 does. A
proven first fill whose record cannot be written returns its section with
no error: the section's `RecordErr` wraps `ErrRecord`, the provider host
logs one line naming the object, and the next fill hashes the whole object
again (INF-MS7).
A decoder reads the region through `RegionReader`, one per goroutine: the
unbuffered reader a stored fill uses (INF-MS1), so the file cache keeps no
copy of the bytes.

## Engine launch

A provider that starts a third-party engine starts it with a clean
environment and confirms where it listens. The shared provider kit
`go/providerhost/enginelaunch` implements both, and
`go/providerhost/enginehttp` is the client that reaches the engine; the
Lemonade, TheNoise and modelhost providers use them, each with its own
variable names and values.

**[INF-EL1] Clean environment.** The engine inherits no variable from the
provider. On Windows its environment holds `SystemRoot` and `windir` (the
kernel's Windows folder), `SystemDrive` (that folder's volume), `PATH` of
System32 alone or of System32, the Windows folder and System32\Wbem,
`TEMP` and `TMP` naming a private folder of the provider, the further
Windows variables the provider names, and the provider's own entries.
Each further variable is computed from the kernel, the process token or the
registry: profile and program folders from the token, ProfileList, the
account's User Shell Folders and `CurrentVersion`, `ComSpec`, `OS`,
`PATHEXT` and the processor identification from the session manager's
environment key, the processor count and architecture from
GetNativeSystemInfo, and the computer, account and domain names from the
system and the token. A registry value is expanded only with variables the
kit computed. A variable that cannot be computed, or whose value names any
other variable or no absolute folder, is left out, and the provider reports
one diagnostic naming the variable, never its value. Elsewhere the
environment is `PATH=/usr/bin:/bin`, `TMPDIR` and the provider's own entries.
An empty value or a name given twice refuses the launch.

**[INF-EL2] No network socket, and the socket's server is the engine.** An
engine that can serve HTTP on an AF_UNIX socket path is given one in a
launch directory the provider creates for that launch alone
(`enginelaunch.NewSocketDir`) under a fixed per-user socket root,
whatever the provider's state folder (`SocketRoot`):
`%LOCALAPPDATA%\oa\s` on Windows, the local application data folder read
from the token's known folder; `$XDG_RUNTIME_DIR/oa` on Linux, or
`/tmp/oa-<uid>` when the runtime directory is unset or is no 0700
directory of this account; `$TMPDIR/oa` on macOS with the same fallback.
On Windows, the kit asks the account profile-view owner whether this
process sees a packaged app's redirected profile. It refuses the default
socket root with `ErrSocketRoot` when the view is virtualized or cannot be
read, before making a launch folder there (VISION 2026-09-17).
For every Windows launch directory, including one under a provider's
explicit root, the kit binds and dials a temporary AF_UNIX socket at the
exact launch path before starting the engine. A connect failure refuses
the launch with `ErrSocketTransport` and removes the directory. This
detects a redirected profile path that accepts a bind but cannot connect.
The kit creates an absent root with the private descriptor below and
refuses a root that is a file or a link, that another principal owns, or
that is open beyond this account and SYSTEM (a DACL that is not protected
or that allows any other principal on Windows; any group or other bit, or
another mode than 0700, elsewhere) (`ErrSocketRoot`, wrapping the helper's
`privatedir.ErrNotPrivate`). The root and each
launch directory are made and checked by OA's one private-folder helper,
abstraction-identity's `go/privatedir` (identity ID-D1), which the span
record folder uses as well (INF-MS7). The launch directory
has a random name, is refused if the path exists, carries a protected
DACL of two entries (this account and SYSTEM) on Windows and mode 0700
elsewhere, and holds the socket `e.sock` and a `held` file its provider
keeps locked (no sharing on Windows, an exclusive `flock` elsewhere) from
creation until the directory is removed. The socket path is printable
ASCII of at most 107 bytes (`MaxSocketPath`); a provider refuses a root
that cannot fit one when it starts (`SocketPathFits`). The engine is given
no port. The provider's client (`enginehttp.NewUnixTransport`) dials the
socket and binds the process the kernel names for its server end
(abstraction-identity ID-T8) before the request's first byte, admits it
only as a member of the engine's contained tree by process id and creation
time, or, without a supervised tree, as the process the provider started,
and rechecks the binding on every read and write; a refused server
receives no byte. A connect error a starting engine gives (the connection
refused, the socket file not made yet, a timeout) is returned as it is;
any other system error, such as the WSAEINVAL a Windows AF_UNIX path in a
packaged app's redirected folder reads, wraps `ErrEngineUnreachable`. Every
engine readiness loop ends once its probe's connects have failed with
`ErrEngineUnreachable` without a break for `UnreachableFor` (3 s), naming the
system error (`UnreachableRun`); the provider kit's `engineproc.Wait`
and the Lemonade and TheNoise providers' own loops keep this rule. Once the engine reports ready over that client, and
again at least every 30 seconds while it runs, the provider reads the
socket owner tables and confirms the engine's contained tree owns no TCP
socket in any state (listening, connecting, established or closing,
outbound connections included) and no bound UDP socket, on any address,
loopback included (`CheckSocketOnly` over identity's `AllSockets`). An
engine that fails is stopped by its own process; a check that fails after
the provider loaded a newer engine for the same model stops only the
engine it checked. The launch directory is removed once the
engine's exit is confirmed, after a stop or a crash. A provider removes
the launch directories under its root that no provider holds and whose
socket answers no dial (`SweepSocketDirs`); a directory whose engine is
still starting is held and stays, so providers can share the root.

An engine that cannot yet serve a socket path keeps the loopback rule: the
provider reads the listener and UDP owner tables once the engine reports
ready, and again while it runs, and confirms the engine's contained tree
holds its TCP listener on 127.0.0.1 at the port the provider chose and no
socket beyond loopback (abstraction-identity ID-T7, `CheckLoopback`), and
every request the provider sends the engine, readiness and work alike,
goes through a client that binds each connection to the engine's tree by
the TCP owner table (`enginehttp.NewClient` or `NewClientWithVerifier`,
ID-T1), so a process that takes the port after the engine exits receives
no byte. These
engines are `sd-server` (stable-diffusion.cpp 462d675, launched by the
Lemonade provider and by the model host's video kind), `whisper-server`
(whisper.cpp v1.8.4), Kokoros `koko` (b17) and `lemond` with the engines
it starts (Lemonade 2026.39.1), and TheNoise (0.9.1);
`research/model-bytes/ENGINE-TRANSPORT.md` records what each needs. A
provider applies both rules through one call, `CheckEngineSockets`, which
takes the socket rule for an engine with no port and the loopback rule
for an engine on a port. An engine that fails either rule, or a platform
without owner tables, fails readiness and the provider stops the tree.

**[INF-EL3] The GPU library path gate.** Before every launch that loads
ROCm's hipBLASLt or rocBLAS kernel files on Windows, the provider runs the
kit's one gate (`enginelaunch.GPUPathGate`) on each library's gfx kernel
folder. With Windows long paths off, every path the library opens, the
folder, a separator, the file and the separators the library adds itself
(one more for hipBLASLt, two for rocBLAS), must fit 259 UTF-16 code units
(`MaxLibraryPath`, research/adoption/thenoise-gpu-crash/FINDINGS.md). A
folder too deep runs only through its 8.3 name in the library's
`*_TENSILE_LIBPATH` variable, and only when that name is shorter, resolves
under the engine root's own final path to the same folder, passes no
reparse point from the root down, and fits; otherwise the launch is
refused (`ErrKernelPathTooLong`). TheNoise's `--device cuda` launch and the
model host's PyTorch engine apply it.

## Model packs

OA's model packs keep 16-bit float weights in an in-house exponent codec
where it loads faster than the disk, and every other tensor as it is
(VISION.md, 2026-10-01; `research/model-locators/GPU-AND-MATRIX-DECOMPRESSION.md`).
The provider kit `go/providerhost/modelpack` implements these rules once. It
sits beside `modelfiles` because the provider host decodes a pack into the
section it lends to the engines it launches (INF-MS1). It moves to
`abstraction-storage` if packs become a storage object kind. The pack index
and the codec are provider-side data: they never reach an application or a
contract field.

**[INF-MP1] Pack index.** An object's packed bytes are a run of encoding
units. Its index records the object's `digest`, `sha256:` and 64 lower-case
hex digits, which binds the index to the object; its decoded `length`; for
each unit in
order, `offset` and `length` of its decoded bytes in the object,
`packed_offset` and `packed` of its stored bytes in the object's packed
bytes, and `encoding`, which is `stored`, `exp-huffman-v1`,
`exp-fixed3-v1` or `exp-ans-v1`, and for a delta unit `transform` and
`base_offset`; `base`, the base object of an index holding delta units
(INF-MP12); and `spans`:
the SHA-256, as 64 lower-case hex digits, of each `modelfiles.ChunkSize`
(16 MiB) span of the decoded bytes. These are the digests a section records
when the same object fills it (INF-MS2); the span size has one owner,
`modelfiles`. A `stored` unit holds its decoded bytes as they are. Units
carry no digest of their own: the decoder refuses a unit whose structure is
broken (INF-MP3), and the span check catches every other change, located to
16 MiB.

Span digests prove bytes only when they come from OA's own record of the
object (INF-MS7): the spans OA recorded when it hashed the object (INF-MS2,
and `Pack`, which hashes the object it packs), or the spans of an index bound
to the object's whole SHA-256 that OA has verified once. The first fill
from a pack whose spans OA has not recorded hashes the whole object against
its digest and then records the spans; later fills trust the recorded
spans. An index read from a pack alone is never proof.

A reader refuses, before it reads a unit, an index whose units do not cover
the object in order without gaps, that names another encoding, whose
`stored` unit has two sizes, whose `exp-huffman-v1` unit decodes to more
than 2 MiB or stores more than its decoded length plus 2,320 bytes (the
16-byte header and two planes' tables of 1,152 bytes), whose
`exp-fixed3-v1` unit decodes to more than 2 MiB or an odd length or stores
more than its headers, chunks and one escape per eight values (INF-MP8),
whose `exp-ans-v1` unit decodes to more than 2 MiB or an odd length or
stores more than its decoded length plus its 16-byte header (INF-MP11),
or span digests that are malformed or not one per span (`ErrCorrupt`).

**[INF-MP2] Choice per unit.** The packer encodes only the bytes of tensors
whose type is `BF16` or `F16`, as safetensors and GGUF name them, in the
encodings whose measured aggregate decode rate for that type exceeds the
measured read rate of the volume holding the pack: `exp-huffman-v1` for
`BF16` and `F16`, each judged by its own rate, and `exp-fixed3-v1` for
`BF16`. With no measured disk rate nothing is encoded. Such a tensor is cut
into units of 512 KiB from its start. Each unit is tried in every such
encoding, and an encoding counts only where the encoded unit is at most 97
percent of its decoded length. The unit takes the encoding with the
shortest expected load, the larger of its stored bytes over the disk rate
and its decoded bytes over the encoding's decode rate, against its decoded
bytes over the disk rate for `stored`; on a tie the smaller encoding. The
expected load is a heuristic from aggregate rates: it orders the encodings
of one unit and predicts no fill's time. The fixed code wins where `exp-huffman-v1` would wait on its decode (few
cores), and `exp-huffman-v1` where its decode keeps up with the disk.
Quantised tensors, `F32`, every other type and the bytes between tensors
are `stored`, in units of at most 16 MiB. The codec refuses any other type, an odd byte count, an empty unit
and a unit over 2 MiB (`ErrDType`). The packer decodes every encoded unit
before writing it and refuses the pack when the unit does not decode to its
bytes (`ErrCorrupt`).

A machine makes its packs when it stages a model, with its own measured
rates. A pack moved to another machine stays valid there and decodes to the
same bytes; that machine may repack it with its local rates.

**[INF-MP3] The `exp-huffman-v1` unit.** Each little-endian 16-bit value
is rotated left by one bit. Its high byte is then a BF16 value's exponent,
or an F16 value's exponent and three highest mantissa bits; its low byte is
the remaining mantissa bits and the sign. The unit is a 16-byte header and
two byte planes: high bytes, then low bytes. The header holds the format
version 1, the type (1 `BF16`, 2 `F16`), each plane's mode (0 raw, 1
Huffman, 2 constant), the value count and each plane's length, the four
numbers little-endian 32-bit. A raw plane is one byte per value; a constant
plane is one byte. A Huffman plane is 128 bytes of code lengths, two 4-bit
lengths per byte for symbols 0 to 255, then 256 little-endian 32-bit
substream end offsets, then the substreams. The code is canonical
(RFC 1951 §3.2.2), complete, of at least two symbols and at most 12 bits
per code. Substream `t` holds the symbols of value quads `t`, `t+256`,
`t+512` and so on, four symbols per quad and fewer in the unit's last
quad. Each substream starts on a byte and holds its codes least
significant bit first; its recorded end offset follows the byte that holds
its last bit. A decoder can start at any substream. A plane is coded only
where its code saves at least 3 percent of the plane. A unit whose bytes
contradict this layout is refused (`ErrCorrupt`).

**[INF-MP4] Decoding into memory.** `Unpack` reads each unit with one
positioned read and decodes it straight into its place in the caller's
memory, the writable view of the section a provider host fills; it creates
no file, temporary or otherwise. The caller passes the span digests apart
from the index, taken from OA's record of the object (INF-MP1). Once every
unit covering a span is decoded, `Unpack` checks that span's SHA-256
against them. A first fill from a pack whose spans OA has not recorded
passes the index's spans and counts the bytes as proven only after the
whole object's SHA-256 matches its digest. A truncated pack, a malformed
unit, or span digests of the wrong count or form are `ErrCorrupt`, a span
of other bytes is `modelfiles.ErrDigest`, and a cancelled context returns
its error; after any refusal the caller discards the memory. Units decode
on several goroutines, and the bytes equal those of one goroutine.

**[INF-MP5] Built and not built.** The decoders run on the CPU in Go's
standard library, and an NPU decodes `exp-fixed3-v1` units inside a fill
(INF-MP10). A GPU decoder of the same bytes follows once the owner
schedules each GPU run. `Pack` and `WriteZip` take the rates as inputs;
`MeasureRates` measures them (INF-MP9). No model host or bridge binds an
archive yet: STORED file entries are regions a container inventory already
lists, and a binding of the packed kind follows with the first engine that
reads a section. `exp-ans-v1` units (INF-MP11) decode wherever a pack is
read; `Pack` writes them as delta units (INF-MP12) and not for plain
weights, where `exp-huffman-v1` stays the default (`ANS.md`). The NAS
shared store, which will hold bases for their deltas, is not built
(`research/model-locators/MODEL-ARCHIVE.md`).

**[INF-MP6] The OA model archive.** A pack and a model archive are one
format: one zip layer (DECISION.md §7 rule 9;
`research/model-locators/MODEL-ARCHIVE.md` §4) of STORED entries. The first
entry is `oa-archive.json`, the index: format `oa-model-archive`, version 1,
and for each file its slash-separated relative `path`, its zip `entry`, its
weight `format` (`safetensors` or `gguf`) when it is one, its pack `index`
(INF-MP1: object digest, length, units, span digests), and for a weight file
its tensor table: name, dtype, shape, decoded offset and length, the tensor's
SHA-256, and for BF16 and F16 tensors the dtype-normalised digest. That
digest is the SHA-256 of the tensor's values widened exactly to
little-endian F32, a NaN's payload and sign bits carried over unquieted, and
it compares values bit for bit: `-0` and `+0` differ. It covers the values
alone; a deduplication by it also matches the shape. It is compared only
with other dtype-normalised digests, never with a tensor's SHA-256. Then
one entry per file, in index order, its data on a 64 KiB boundary padded
through a zipalign extra field (ID `0xD935`) in the local header only. The
extra field stays under 65,536 bytes: where the padding would pass that,
the excess, at most the field's 6 to 26 fixed bytes, goes as zero bytes
before the local header, which readers find through the central directory. A file whose units all stay stored is
written under its own path, and every zip tool extracts it as the file; a
file with an encoded unit is written as `<path>.oa-units`. Paths are
relative, slash-separated UTF-8 that Windows, macOS and Linux extract as
written: no empty, `.` or `..` element; no backslash, colon, control
character or any of `<>"|?*`; no element ending in a dot or a space; no
element named, before its first dot, `CON`, `PRN`, `AUX`, `NUL`, `CONIN$`,
`CONOUT$`, `COM0` to `COM9`, `LPT0` to `LPT9`, or `COM` or `LPT` followed
by `¹`, `²` or `³`, in any letter case. Every path and entry name is
unique in any letter case. The archive's own zip writer writes Zip64 fields where a size,
an offset or the entry count needs them. `WriteZip` encodes each file twice,
first to learn its entry's size and CRC-32 and the index that precedes the
entries, refuses the archive if the two encodings differ, and gives each
weight file's span record to `SpanRecords` (INF-MS7). `ReadZip` accepts an
archive whose entries are all STORED and unencrypted, whose index is present,
at most 64 MiB, of this format and version, parses with no unknown field and
names every other entry once; whose entry names, and whose paths and entry
names, are unique in any letter case; whose files have such paths, the entry their
units call for, data on the 64 KiB boundary, an index whole by INF-MP1 that
uses exactly the entry's bytes, and tensors inside the file
(`ErrCorrupt`); it reads no file's data. Go's `archive/zip`, bsdtar (`tar`)
and PowerShell's `Expand-Archive` list such an archive and extract every
file entry, Zip64 fields included.

**[INF-MP7] Fill from an archive.** `FillZip` needs OA's span records and
refuses without them. It reads and checks the whole index (INF-MP6) and
refuses an archive holding no file of the object's digest
(`modelfiles.ErrDigest`) before a section exists or a unit is read. It then
fills a section through `FillDecoded` (INF-MS8): each goroutine reads units
through its own `RegionReader`, one positioned read per unit, and decodes
them straight into the section. An `exp-fixed3-v1` unit is decoded with
each escaped value at exponent `b + 7`, which no value inside its chunk's
window has, and `FillDecoded`'s hash pass applies the unit's escapes to
each block just before hashing it; the one value whose two bytes lie in two
spans takes its escape at decode. The decoder records where it found
each escape, its value's index within its chunk, in value order, and each
chunk's first escape, which equals the index the chunk header's bytes 4 to
7 hold; the pass visits those values alone and writes one only where it
carries its chunk's mark `b + 7`, so its cost follows the escapes. A wrong
or altered position list leaves an escape at its mark or a value
unpatched, and the hash refuses those bytes; the list is never trusted
beyond what the proof shows. That pass is the one check of the bytes:
against OA's record of the object when one exists, and against its whole
digest on a first fill. The fill holds each such unit's window bases,
escapes and escape positions until every span holding its bytes has been
passed, then drops them. The bound is the escapes' share: one escape per
eight values at most, one escape byte and a two-byte position each, so at
most 3/16 of the decoded size plus 5 bytes per 4,096 values for bases and
chunk starts. A hostile pack reaches that bound; Qwen2.5-1.5B at 2.94
percent escapes holds 0.7 bytes per sixteen. It
writes no file; `release` is called exactly once, as for `Fill`.

**[INF-MP8] The `exp-fixed3-v1` unit.** BF16 only. Each value's exponent
is coded in 3 fixed bits as an offset into a window of seven exponents
chosen per 4,096 values, and code 7 escapes it: ZipServ's TBE code
(https://arxiv.org/html/2603.17435), in the chunk layout measured on the
NPU in
`GPU-AND-MATRIX-DECOMPRESSION.md` §12.2. A unit is a 64-byte header
(format version 1, type 1 `BF16`, then the value count and the escape
count as little-endian 32-bit numbers at bytes 4 and 8, every other byte
zero), then one 5,696-byte chunk per 4,096 values, the last one padded,
then the escapes. A chunk is a 64-byte header whose first byte is the
window base `b`, at most 248, whose bytes 4 to 7 hold the index of its
first escape in the unit's escape list, the number of escapes the unit's
earlier chunks use, as a little-endian 32-bit number, and whose other bytes
are zero; three 512-byte bit planes; and 4,096 bytes of
`sign << 7 | mantissa`. A decoder can start at any chunk, and NPU tiles can
share one unit's chunks. Value
`v = 512·blk + 64·i + j` of a chunk keeps bit `p` of its code in bit `i` of
byte `64·blk + j` of plane `p`. The exponent is `b + code` for codes 0 to 6;
code 7 takes the next escape byte, in value order. The encoder picks per
chunk the window that covers the most values and refuses a unit with more
than one escape per eight values, which `Pack` then encodes as
`exp-huffman-v1` or stores (INF-MP2). A unit whose bytes contradict this
layout, whose chunk header names another escape index, or whose escapes
do not match its codes, is refused (`ErrCorrupt`).

**[INF-MP9] The pack command.** `openabstractions models pack` makes and
reads model archives (INF-MP6) from files on this machine and reaches no
service. It writes one archive of one model file, or of every file under a
folder. It reads each weight file's safetensors JSON header or GGUF tensor
table (`modelpack.ReadTensors`: names, dtypes, shapes and ranges; GGUF
tensors of type F32, F16 and BF16 by element count, every other byte left to
stored runs), maps every file read-only, and packs weight files with the
per-unit choice of INF-MP2 from `--rates` given in GB/s or from rates it
measures first (`modelpack.MeasureRates`: each encoding's decode rate on
synthetic weights in memory, and the read rate of the largest weight
file's volume through the stored fill's unbuffered reader, at most 1 GiB,
within `--seconds`, its clock started once the file is open, each reader
reading at least one 16 MiB block, and a file under 16 MiB read whole once
as one shorter block). Each measurement repeats its decodes, or reads the
file again from its start, until at least 5 ms, or a quarter of
`--seconds` when that is shorter, has passed and the clock has moved.
Every weight file's header is read before any measurement, and an empty or
invalid one refuses the run first. Weight files under 64 MiB in all, none of them
16 MiB, are packed without a measurement, every unit stored, and the run
says so: such an input loads in tens of milliseconds, an encoding would
save a few, and the measurement takes `--seconds`. A measured rate that is
not positive and finite fails the run (exit 1). `measure` on a file under
16 MiB reads it whole once. Every other file is stored as it is. Weight files are
`.safetensors`, `.sft` and `.gguf`; a pickle (`.pth`, `.pt`, `.ckpt`, `.bin`,
`.pkl`, `.pickle`) refuses the whole run before anything is written. The
span record of each weight file is written only into a folder `--records`
names (INF-MS7); the provider host's own first fill records it otherwise.
`info` prints each file's digest, size, bytes kept, and units per encoding;
`verify` decodes every file in memory, checks every span and every file's
digest against the index, and that the archive holds the object `--digest`
names, and writes nothing; `restore` writes the original files byte for
byte, checked as they are written (`modelpack.RestoreFile` and `Restore`):
one file to the path given, several into a folder that must be absent or
empty. No command overwrites a file. The archive `pack` writes and each
file `restore` writes go under a temporary name in the target's folder and
are renamed to the target at the end without replacing anything: Windows
`MoveFileW` without replace, Linux `renameat2` with `RENAME_NOREPLACE`,
macOS `renamex_np` with `RENAME_EXCL`, and on other systems, or where the
filesystem refuses that flag, a hard link and then removal of the
temporary name. The target never holds part of a file, and the temporary
file is removed on any failure. A restore that fails removes every file and folder it made. Exit
status 0 done, 1 a file could not be read or written, 2 usage, 3 refused (a
pickle, a weight file that is not safetensors or GGUF, an archive that fails
verification, an output path where something already stands).

**[INF-MP10] A device beside the CPU.** `FillZipWith` takes an optional
`modelpack.FixedDevice`, a batch decoder of `exp-fixed3-v1` units such as
the NPU decoder in `providerhost/npudecode`. A batch is a fixed number of
slots of `FixedSlot` bytes (397,376, the largest unit of 512 KiB); the
device decodes each slot's 64 chunks with every value's exponent `b +
code`, so an escaped value leaves at `b + 7` as the fused CPU decode leaves
it. A fill uses the device when the archive file holds units of at most
512 KiB in that encoding, the device reports no error, and no other fill
holds it; otherwise the CPU decodes every unit. Helpers read each batch's
units into a free buffer set and check each one on the CPU as the decoder
checks it, header, window bases, escape indexes and escape count included,
recording its escape positions as the decoder does, before the device
runs; a unit that fails is refused (`ErrCorrupt`) as on
the CPU. While the device decodes one batch the helpers read the next and
copy the previous one into the section; the CPU workers decode every other
unit meanwhile. Escapes stay in the hash pass (INF-MP7), and the one value
split across two spans takes its escape when copied. The proof is the
fill's, unchanged: every span is hashed against OA's record or the whole
digest (INF-MS8), and nothing the device writes counts until it passes.
A device error, a timeout included, hands that batch and every later one
to the CPU, which reads and decodes them again; the error is reported
(`FillReport.DeviceErr`) and logged, and the device refuses every later
run. When the proof refuses a fill the device took part in, the fill runs
once more on the CPU alone; if that fill is proven the device is
distrusted for the life of the process. An archive rewritten between the
two fills can distrust a healthy device that way; that cost is accepted.
`release` is called once either way. `FillReport` gives units per decoder,
device runs, the decode phase's time and the time spent in device runs.
A device is used only after `CheckFixedDevice`, its self-test, decoded
known units with escapes and a padded chunk in every slot of every buffer
set bit-exact against the CPU decode. `npudecode.Open` runs it before it
returns a device. The kit is trusted for code: whatever can write a kit
file runs code in the provider host. `npudecode` therefore refuses any kit
file, `kit.json` included, that identity ID-D2 refuses
(`trustedpath.CheckFile` with `MachineOwners()` and `Account`): a file or
a folder above it up to the volume root owned by anyone but SYSTEM,
Administrators, TrustedInstaller or this account; a right for anyone else
to change, delete or re-permission the file, or to delete, rename or
re-permission a folder on its path; or a reparse point (link, junction or
mount point) anywhere on the path. A kit an installer placed under Program
Files passes, as does one this account built in a folder whose whole path
passes. ID-D2 covers the link and junction checks; it does not compare the
path a handle resolves to, so `npudecode` also refuses a kit file whose
open handle's final path is not its own (a mapped or substituted drive).
It reads `oa_npu_decode.dll`, the design files and
`fixed3.json` through handles that deny writes, checks each against the
SHA-256 `kit.json` pins (and `kit.json` against a pin the caller holds, when
given), and loads the shim by its absolute path with `LoadLibraryEx` and
`LOAD_LIBRARY_SEARCH_SYSTEM32` while its handle is still open, so the
library loaded is the one checked and its imports come from System32. It
refuses to load anything while `XRT_INI_PATH` or `XILINX_XRT` is set or an
`xrt.ini` stands in the working directory or beside the executable, since
XRT reads plugins from that configuration (`ErrXRTConfig`). The shim's four
calls (query, open, decode, close) catch every exception and reach the NPU
driver's `xrt_coreutil.dll` (XRT, Apache-2.0) through `syscall`. The
design's `fixed3.json` must name modelpack's slot and unit sizes. The
decoder runs one batch at a time with a time limit (500 ms; a batch of 64
units takes about 1.2 ms), aborts a run that exceeds it, and never frees
the buffers of a run that timed out. `Close` waits until no fill holds the
device, then frees it, and no fill can claim it after. It is Windows only;
elsewhere `Open` reports `errors.ErrUnsupported`. The design's sources and build scripts
are in `npudecode/native`; built files stay outside OA's trees until the
owner decides they ship (`research/security-reviews/npu-toolchain-iron-1.4.3.md`
condition 5). Measured in
`research/model-locators/GPU-AND-MATRIX-DECOMPRESSION.md` §14. No
provider host passes a device yet, since none fills from an archive.

**[INF-MP11] The `exp-ans-v1` unit.** OA's own static rANS coder
(VISION 2026-10-02; `research/model-locators/ANS.md`), after Duda's
asymmetric numeral systems, Giesen's `ryg_rans` and his interleaved
coders; no third-party code. A unit cuts its 16-bit values into two byte
planes. It is a 16-byte header (format version 1; the layout; the high and
the low plane's modes; then the value count and the two planes' sizes as
little-endian 32-bit numbers at bytes 4, 8 and 12), the high plane and the
low plane. Layout 1 (`BF16`) and 2 (`F16`) rotate each value left by one
bit, as `exp-huffman-v1` does, so the high plane of BF16 is the exponent.
Layout 3 holds a delta's 16-bit words against a base, high and low bytes
as they are, and needs the unit's base values to decode. A plane is raw
(mode 0), one byte for every value (2), rANS with one table (3), or, in
layout 3 only, rANS with one table per context (4), where a value's context
is its base value's BF16 exponent minus the plane's window start, clamped to
the window's contexts, at most 32. A table gives each present byte a
frequency out of 4,096, written as unsigned varints: for each present byte
the count of absent bytes before it and its frequency minus one, then the
count of absent bytes to the end; a context no value falls in has an empty
table. The stream is little-endian 16-bit words: four 32-bit states, low
word first, then the words the states read when they fall below 2^16. Value
i is coded with state i mod 4, each state in [2^16, 2^32), and every state
returns to 2^16 at the end. A plane is coded only where that saves 3
percent of it. A unit whose layout, mode, sizes or tables contradict this,
whose stream runs out or ends with a state other than 2^16 or with words
left, or a layout-3 unit decoded without a base of its length, is refused
(`ErrCorrupt`). A changed raw plane decodes to other bytes, which the span
check refuses as for every unit.

**[INF-MP12] Delta units against a base.** VISION 2026-10-02;
`research/model-locators/DELTA.md`, `ANS.md`. A fine-tune's unit may be
stored as a delta against the same values of a base object: the
`int-diff-v1` transform takes each BF16 value's order-preserving key (the
sign bit flipped for a positive value, every bit for a negative one) minus
the base value's key, modulo 2^16, zig-zag coded, and codes the words as an
`exp-ans-v1` unit in its delta layout, under contexts from the base values'
exponents (INF-MP11). The decoder undoes the difference as it joins the
planes. The index names the base once per file, `base` with its `digest` and
`length`, one level deep, and each delta unit its `transform`
(`int-diff-v1`) and `base_offset`, the decoded offset of its `length` bytes
in the base (any byte; tensors need not be aligned). A reader refuses
(`ErrCorrupt`) a delta unit with another transform or encoding, an odd
length, bytes outside the base, or no base in its index; an index whose base
digest is malformed or whose length is not positive; a base offset on a unit
with no transform; and a base named by an index that holds no delta unit.
`PackDelta`, and `WriteZip` for a source that names a `DeltaBase`, tries each
unit of a BF16 tensor the base holds by the same name, dtype and length as a
delta and keeps it where it is smaller than the unit's other choice and saves
3 percent; a delta is chosen for disk and download size. Every delta unit is
decoded against its base before it is written. The base must be a proven
object before any delta unit decodes. `UnpackWithBase`, `RestoreWithBase` and
`RestoreFileWithBase` take a `ProvenBase`, which `ProveBase` makes by hashing
the whole base once and retaining the caller's byte slice without copying it.
This is a borrowed in-process value, not an owned immutable snapshot: its
caller must keep the backing storage alive and unchanged through every alias,
from entry to `ProveBase` until every operation using the result has returned.
That includes calls using copied `ProvenBase` values concurrently. The digest
is not rechecked as bytes are read. Mutating the source after proof violates
this precondition and can stale the identity claim, even when changed bytes
are outside the delta units' read set and the fine-tune output remains
correct. `FillZipWith` takes the base's section in
`FillOptions.Base`, proved when it was filled (INF-MS2, INF-MS8). Before a
section is made or a unit read, a delta object with no base is refused
(`ErrBase`), and one whose base is another object by digest or length is
refused (`modelfiles.ErrDigest`). The fill reads the base section through a
loan of its own, mapped read-only, so the base stays in memory until the last
delta unit is read; the loan's end frees a base no engine holds, as any
section is freed at its last loan's end (INF-MS5). A caller that uses the
base section after a fill, or fills further deltas from it, therefore keeps a
loan of its own on that section for as long as it does; nothing else keeps it
until the residency broker owns base residency. The fine-tune is proved as
every fill is: span by span against OA's record of it, or whole against its
digest on a first fill. A base matching the index's digest but differing from
the base used when packing can still produce the same fine-tune when those
differences are outside the delta units' read set. When referenced bytes
change decoded output, the trusted fine-tune proof refuses it; changed base
exponent contexts may cause `DecodeDelta` to refuse earlier with `ErrCorrupt`
naming both causes ("or not the base this delta was made from").
`openabstractions models pack --base <file or archive>` packs one
weight file against a base, given as a safetensors file or as an archive
holding one weight file that is not itself a delta. `verify` and `restore`
take the same `--base`, refuse a delta without it and refuse another base
(exit 3). `info` names each file's base. A CLI base stays unchanged and
available through the last operation that reads it; on Windows its source
file stays open with writes and deletes denied, and on Unix it is read into a
private copy. The CLI releases that mapping or buffer only after the last
use. The fill path takes an explicit section loan and a read-only mapping
until the last delta unit has been decoded. The decoded fine-tune still has
its own proof: recorded 16 MiB spans, or its whole digest on a first fill.
Restore checks the whole digest at completion against the index; as with any
pack, that alone verifies the pack's claimed bytes, and the caller compares
an independently known digest when identity matters (INF-MP1). A wrong
immutable base whose digest is supplied by a caller is rejected by the
fine-tune's trusted span or digest proof if it changes decoded output. On a
failed restore, output may be partial and the caller must discard it; the CLI
restores to a temporary file and removes it on failure.

`ModelArchive.Bases()` reports base dependencies, but no archive store or
inventory consumes it today; modelpack only builds and reads archives. The
future shared archive-store removal/retention path is the single meeting
point that must retain a base while a stored delta names it and release the
dependency when that archive is removed. A delta whose base is absent is
fetched as the whole fine-tune (VISION 2026-10-02 (3)); there is no current
store-level retention behavior to qualify. The independent caller that uses
a base section across fills keeps its own loan until done. Costs are bounded
per unit (2 MiB decoded; for exp-ans-v1 at most 16 bytes of packed overhead)
and Restore decodes in a 256 MiB window (INF-MP1, INF-MP9); there is no
contract total-object-size cap. ProveBase's one full SHA-256 pass is O(base
length), but it adds no base-sized copy and no per-unit whole-base hash.

## llama.cpp

A provider that starts llama-server enforces the conditions of the llama.cpp
b10825 security review
(`research/security-reviews/llama.cpp-b10825-win-vulkan-x64.md`) that concern
its options, its environment and the media it is sent. The shared provider
kit `go/providerhost/llamacpp` holds them once; the Lemonade and modelhost
providers use it, and each may refuse more on top.

**[INF-LC1] Forbidden options.** The provider never passes, and refuses in
any configured or requested argument, the options of review condition 2 in
every spelling b10825 accepts, alone or as `--option=value`:
`--sleep-idle-seconds`, `--host`, `--rpc`, `--tools`, `--tools-runtime`,
`--agent`, the MCP options, `-hf` and its `--hf-*` forms, `--model-url`,
`--mmproj-url`, `--docker-repo`, `--spec-draft-hf`, `--models-dir`,
`--models-preset`, `--media-path`, `--path`, `--ui-config-file`,
`--slot-save-path`, `--log-file`, `--log-prompts-dir`, `--video-ffmpeg-dir`
and `--cors-origins`. A provider's own allowlist of options a person may add
admits none of these spellings.

**[INF-LC2] Forbidden environment names.** llama-server never receives a
variable named `GGML_*`, `MTMD_*`, `LLAMA_*`, `HF_*`, `HTTP_PROXY`,
`HTTPS_PROXY`, `ALL_PROXY` or `NO_PROXY`, in any letter case, whatever the
provider's allowlist says. `LLAMA_API_KEY` is the one exception: a provider
that makes a per-launch key passes it under that name, and a provider that
makes none refuses it.

**[INF-LC3] Chat media.** A chat content part reaches llama-server only as
text, as an image the provider decoded and encoded itself, or, where the
provider admits audio, as audio it recognised. An image URL must be a
base64 data URL declaring `image/png` or `image/jpeg` whose bytes are that
format, at most 16 MP by its header before any pixel is decoded; it is
replaced by the provider's own 8-bit NRGBA PNG data URL. Audio data must be
WAV, MP3 or FLAC by its leading bytes, given as OpenAI's raw base64 in
`input_audio.data` or as a base64 data URL declaring `audio/*`; it is
replaced by the provider's own standard base64 of the same bytes, raw in
`data` and as a data URL naming the detected type in `url`. An http(s) URL,
a `file:` URL, video, WebP, GIF, BMP, any other image or part type, and data
that does not decode are refused before the request leaves the provider.

## Video generation

Video is durable work: job kind `inference` with profile `video`
(`job.thrift`), executed by the runtime's job executor. The engine lives
behind a native provider, which serves `video@1` to its designated runtime;
the runtime holds the provider's operation id and nothing of the engine.
`INF-G2` and `INF-G4` stay reserved ([Next](#next-video-generation)).

**[INF-G1] A video request.** A video job carries an image-shaped request
with `count` 1 and `duration_ms` 1..600000: `generate` makes the clip from
the prompt, and `edit` names one authorized PNG, JPEG or WebP initial frame
and carries no mask. The request names a model, whose preset the provider
applies (INF-G8). `chat@1`, `image@1` and `image@2` carry no video.

**[INF-G3] A video result is one local delivery.** The executor copies the
provider's result into the caller's authorized content store and returns one
`Delivery` with location `local`, its SHA-256 digest and size, and media type
`video/mp4`, `video/webm` or `video/avi`. These are the names Go's `net/http`
sniffer gives the bytes, and the declared type MUST equal the sniffed one; a
provider's `video/x-msvideo` is read as `video/avi`. The result is at most the
operation output bound (`MaxImageOutputBytes`, 64 MiB by default) and carries
any audio track inside its container. The clip of an `edit` video job starts
from its one image, which is the first frame (INF-G1). A video job refuses an
image result and an image batch refuses a video result, each failing as
`upstream:output`.

**[INF-G5] A native provider serves video through `video@1`.** A provider
declares router profile `video`, with its models as `video:<model>` entries
(router CONTRACT.md ROUTE-D9), and serves `Video` (`Start`, `Observe`,
`Cancel`). The runtime offers no `video@1` endpoint to applications; they
submit video as a job. The executor tries its backends in order and
takes the first whose profile selects a host: a video job for a model a
native video provider lists goes to that provider, and any other video job or
image batch goes to an image host as before. Admission decides
`abstraction.inference/complete` on `host:<name>`, as image work does. The
runtime forwards model, prompt, size, `duration_ms`, the caller's claimed
program and `request_id`, the job's operation id; it stages an initial frame
with the provider's `content-writer@1` before `Start`, and reads the result
with its `content-reader@1`. A provider MUST treat a `Start` repeating a
`request_id` it holds as the same operation: it starts nothing, ignores the
request's frame and returns that operation's admission. For any other
`Start` that names an `image_digest`, a provider MUST refuse the request as
`invalid` with reason `content:unknown` when the digest is not in its
content store, and MUST NOT generate the clip from the prompt alone. A
provider MUST hold
each `request_id` and the operation it names until at least 24 hours after
that operation ends; a restart drops them (INF-G7). A provider that refuses
`Start` ran nothing, and the job fails at once as `native:<outcome>:<reason>`.
A provider that answers `Observe` with `forbidden` ends the job as
`native:forbidden`. The service host serves `video@1` beside the provider's
other profiles and content services when the provider assigns a
`VideoServer`; it binds each call's peer as it binds chat and answers
`forbidden` itself for a caller other than the designated runtime, and for
every caller on an endpoint that designates none.

**[INF-G6] Progress reaches the job snapshot.** While the provider generates,
`OperationSnapshot.progress` is its step of steps; a provider that knows only
a fraction reports it in its own steps. The runtime reads a provider that
reports steps under way every second, and queued or silent work every five
seconds, since `video@1`'s `Observe` has no wait for a change. While the engine holds the clip
behind other work, the snapshot's waiting word (JOB-A15) is `queue:<n>`, `1`
for next, or `queued` when the position is unknown. A job this runtime holds
back because its server is busy with the runtime's other jobs shows
`host:busy` before it is sent (INF-G9). A generating or ended job carries no
waiting word. A completed job reads progress 1 of 1.

**[INF-G7] A lost video fails visibly; a lost `Start` reply is
reconciled.** A provider's video operations live in its memory. When
`Observe` answers `unknown` for an operation the job recorded, because the
provider or its engine restarted, the job fails with `provider:lost` and the
work is never sent again.

Submission and its reconciliation record `upstream:uncertain`. A `Start`
whose reply was lost, or a first-frame transfer that failed after a call
reached the provider or was interrupted, leaves the job waiting as
`upstream:uncertain` under `recoverable-upstream@1`. Only a refusal made
before any call reached the provider fails a first submission at once. The
executor then sends the same `Start`, with the same `request_id`, until a
provider answers, and records the operation the answer names. It waits 1
second before the first resend and doubles the wait after each unanswered
one, up to 60 seconds; an answer resets the wait. The resend stages the frame
again when the caller can still read it, and otherwise names it unstaged. A
provider that held the `request_id` returns the operation it already admitted
and ignores the frame. A provider that restarted before answering holds
nothing and admits the work once as new, since the earlier attempt ended with
its process. When that provider's store no longer holds the frame either, it
refuses the resend and the job fails as `native:invalid:content:unknown`. A
cancel requested while the job is uncertain is carried out after the next
answer: the executor sends `Start` to learn the operation and then calls
`Cancel` on it. When 24 hours have passed since the first `Start` without an
answer, a provider returning later no longer recognises the `request_id`,
and the executor ends a cancelled job locally as `cancelled` with reason
`upstream:unanswered`.

Observation records `upstream:unavailable`. A `Poll` of a recorded operation
that cannot reach the provider leaves the job waiting as
`upstream:unavailable`, and the executor observes it again.

**[INF-G8] The provider owns the preset.** A video provider applies the
preset it keeps for the requested model: sampling steps, guidance scale, flow
shift, scheduler and decoder. No caller field or extension overrides any of
them. A provider refuses a required extension naming one as
`unsupported_feature` and ignores an optional one. A caller that wants
another preset names another model.

**[INF-G9] Jobs take turns at a model server.** The executor runs each
claimed inference job, video or image batch, in its own task under the
job's lease. It executes at most one job at a time at one local model
server and at most four at one hosted service, each counted by the server's
name; `JobExecution.SetHostConcurrency` changes both limits. A newly
submitted job that finds its server's limit reached waits, claimed and
renewing its lease, in the order it was claimed, and its snapshot's waiting
word is `host:busy` (JOB-A15). A cancel requested while it waits ends it
`cancelled` with nothing sent upstream. A job that gets its turn is planned
again before it is sent: rights, credentials and the server are decided
then, and a refusal at that point ends it and passes the turn on. At
shutdown a waiting or running job lets its lease go with the waiting word
`service:stopped`. A recovered job that may already run upstream counts
against its server at once without waiting, and can take the count past the
limit. A job whose lease renewal has not succeeded within the lease's
duration stops, gives its server turn to the next job and lets its lease
go, and another runtime may then claim it. Every wait on a renewal ends by
that deadline, at shutdown too; a renewal that hangs in the job store is
left to return in the background, one store call per job at a time. The limit counts this runtime's executions. A job that let its lease go
can still run upstream, a native video provider queues work of its own
(INF-G6), and other runtimes count their own jobs. The budget check (INF-C2)
runs for each attempt, and concurrent hosted jobs can overshoot a daily
budget by the number in flight.

**[INF-G10] A job outlives its server's declaration once.** A job that was
sent records its server's binding. When that server's declaration is
replaced (same name, another binding) the server cannot know the job's
operation, and when it is gone or not servable the job waits for it as
`host:unavailable` for at most 30 minutes, counted from the first time it is
found gone in the current outage; a server that answers again ends the
outage, and a later one counts afresh. Then, or at once for a replacement, a job that spends no
credential is planned afresh once: its record is cleared and marked
replanned, it waits as `host:replaced:replanned` or `host:gone:replanned`
until the next sweep, and it is routed and sent again with its same
`request_id` to whatever server serves its model now, its admission, rights,
input and output decided again. When none serves it, the plan fails it with
the router's own reason. A job already planned afresh whose server changes
again, and a job that spends a credential, whose resend could be charged
twice, fail as `host:replaced` or `host:gone` and are never sent again. A
legacy record without a binding keeps waiting as `host:unbound`. A local
provider that was not stopped when its declaration changed may still run
the first attempt, which is local compute only; the job delivers the second.

## Image output

**[INF-IO1] An engine's raw pixels become the provider's own PNG.** A
provider whose engine returns raw pixels writes the image with the kit's
`providerhost/rgbpng`, Go's standard library encoder, never with an encoder
the engine bundles. Pixels are RGB24 rows, top to bottom with no padding, of
1 to 8,192 on each side; a buffer of any other length is refused before
anything is written (`ErrPixels`). The PNG is 8-bit truecolour without
alpha and decodes to exactly the pixels given; its media type is
`image/png`. The provider publishes it through its content store's
`DesignatedContent.OutputWriter`, the one preparer of generated-output
commits every provider the kit composes uses: a designated runtime's
subject only, a bound per profile and media type, an image's media type the
one its bytes sniff as.

## Image edits

An `image@1` or `image@2` edit names one input image by digest and may name
one mask by digest (VISION 2026-10-01, request input and mask transparency).

**[INF-I1] Edit mask.** A mask is a PNG of the input image's pixel size. A
fully transparent pixel, alpha 0, marks the area the model may change; every
other pixel marks the area it keeps, whatever its colour or partial
transparency. Once both inputs are read, inference refuses a mask whose
header names another pixel size than the input image's, `invalid` with reason
`mask` at step 5 of INF-A2. It then decodes the mask under the bound of the
route that sends it: Stability's documented edit limit on its route (each side
at least 64 pixels, 4,096 to 9,437,184 pixels, aspect ratio between 1:2.5 and
2.5:1) and stable-diffusion.cpp's (INF-SD2) on an OpenAI-compatible route and
an image engine. A mask beyond the bound is `unsupported_feature` with reason
`size:engine-bound`; one whose pixels do not decode, or that marks no pixel to
change, is `invalid` with reason `mask`. The native relay compares the sizes
only, and the receiving provider decodes the mask under its own route's bound;
the remote relay leaves every check to the runtime that reads the mask. Each
route that carries a mask has one declared form, converted by the provider
kit's `imagemask` package and tested against this rule: an
OpenAI-compatible edit receives the alpha form (alpha 0 on the area to
change, painted white, and every other pixel opaque black, so a server
behind the route that reads luminance, such as `sd-server`, reads the same
area); Stability's inpaint route receives a grayscale mask, white on the area
to change; the native and remote relays pass the contract mask by digest to
the service that converts it; the gateway's OpenAI-style edit upload is
already this form. A provider that sends a mask on converts it to its
engine's form and may check the size again. An application converts its own
mask form, such as ComfyUI's `MASK` where 1 marks the area to change, into
this one.

## stable-diffusion.cpp

Image work may reach stable-diffusion.cpp's `sd-server` through any
OpenAI-compatible model server: Lemonade runs it for its image routes, and
`sd-server` serves the OpenAI routes itself. The conditions of the
stable-diffusion.cpp master-843-462d675 review and its video addendum
(`research/security-reviews/stable-diffusion.cpp-master-843-462d675*.md`)
that concern request text and size are held once in the provider kit
`go/providerhost/sdcpp`. Inference applies them to every image request, and
the Lemonade provider applies them again to each body it forwards.

**[INF-SD1] Engine directive.** `sd-server`'s `/v1/images/*` and
`/sdapi/v1/*` routes read generation parameters, LoRA file paths included,
from a JSON block between `<sd_cpp_extra_args>` and its closing tag in the
prompt. An image request whose prompt or any extension value carries the
opening tag `<sd_cpp_extra_args` is `invalid` with reason
`prompt:engine-directive` at step 1 of INF-A2, whichever model server would
serve it. The match ignores letter case and folds full-width forms
(U+FF01..U+FF5E), the small less-than sign and low lines (U+FE64,
U+FE4D..U+FE4F), the long s, dotless i and Kelvin sign, and drops format
characters (category Cf) and non-spacing marks (Mn) before comparing. A
provider forwarding image work toward `sd-server` refuses the same tag in
every text value of the body; that refusal ends the request `invalid` with
the same reason.

**[INF-SD2] Image size.** `sd-server` decodes request images against the
request's width and height and allocates their product in C `int`
arithmetic; with size `auto` it takes the size from the input image. An
OpenAI-compatible model server receives a size of 64..4096 on each side and
at most 4,194,304 pixels (2048 × 2048); a larger size is
`unsupported_feature` with reason `size:<width>x<height>` at step 3. Each
edit input sent to such a server is a PNG, JPEG or WebP whose header, read
before any pixel is decoded, names at most 4096 on each side and 4,194,304
pixels. A larger input is `unsupported_feature` with reason
`size:engine-bound` at step 5, and a header that does not parse as the
declared type is `invalid` with `content:media-type`. A provider's
forwarding refusal of a size or input beyond that bound ends the request
`unsupported_feature` with reason `size:engine-bound`.

**[INF-SD3] Request form.** A provider forwarding image work toward
`sd-server` forwards only the fields inference's OpenAI-compatible adapter
sends (`model`, `prompt`, `n`, `size`, `response_format`, the `seed` its
`Config.ImageFields` writes as an integer 0..2147483647 (INF-A4), and the
edit files `image` and `mask`), each once, as its own encoding of the checked
values. `sd-server` reads a mask's luminance, white marking the area to
change, and ignores alpha; the Lemonade provider converts an INF-I1 mask to
that form.
Any other field or part, a repeated or transfer-encoded part, and a body
that does not parse end the request `invalid` with reason
`request:engine-form`. It reads a body only while it holds its request slot,
bounded by two input images of `Config.MaxImageBytes` and the text fields.
It serves no model that resolves to a Lemonade collection: an Omni
collection's tool calls send `generate_image` and `edit_image` to
`sd-server` past these checks.

A provider that launches `sd-server` itself with a video model applies the
video addendum's launch and request conditions, also held in
`go/providerhost/sdcpp`:

**[INF-SD4] Command line.** The provider builds `sd-server`'s arguments from
typed fields (`sdcpp.Args`): `--listen-ip 127.0.0.1` always, the port it
chose, `--diffusion-model`, `--t5xxl` and `--tae` naming its pinned files,
and only the reviewed device, memory and logging options (`--fa`,
`--diffusion-fa`, `--diffusion-conv-direct`, `--vae-conv-direct`,
`--offload-to-cpu`, `--auto-fit`, `--max-vram` with one number,
`--backend` with `cpu`, `vulkan0`..`vulkan7` or module assignments of them,
`-t`, `--vae-tiling`, `--temporal-tiling`, `--vae-tile-size`, `-v`,
`--color`). `sdcpp.CheckFlag` refuses the addendum's condition 4 options by
name in every spelling, `-M`/`--mode`, `--rpc-servers`, `--serve-html-path`,
the directory options, every other model path, `--vae`, `--mmap`, every
default-image and prompt-file option, `--hires*`, `--sigmas`, the cache
options and `-l`/`--listen-ip` other than `127.0.0.1` among them, before any
reviewed option is read.

**[INF-SD5] Environment.** The engine starts from the provider kit's clean
environment (INF-EL1), and `sdcpp.ForbiddenEnvironment` refuses names
starting `GGML_`, `SD_`, `VK_` or `HF_` and the proxy variables in any letter
case even if one were added to the provider's allowlist.

**[INF-SD6] Video request.** Every body sent to `/sdcpp/v1/vid_gen` is built
by `sdcpp.BuildVideoRequest` from the provider's preset and checked by
`sdcpp.CheckVideoRequest` before it leaves: one JSON object with only
`prompt`, `negative_prompt`, `width`, `height`, `video_frames`, `fps`,
`seed`, `sample_params` (`sample_steps`, `sample_method`, `scheduler`,
`flow_shift`, `guidance.txt_cfg`, and `custom_sigmas` when the preset sets
its own table), `init_image`, `output_format` and `output_compression`,
each once. The size is one of 832×480, 480×832, 1280×704, 704×1280 and
512×512, and a provider's model set may narrow it; `video_frames` is 4n+1
in 5..121; `fps` 1..30; `sample_steps` 1..8; `txt_cfg` 0..10;
`flow_shift` 0..20; `custom_sigmas` is `sample_steps`+1 values in 0..1,
strictly decreasing and ending at 0, set by the preset alone; each prompt at
most 2,048 bytes of UTF-8 with no control character or line break (the
engine echoes it into its log); `output_format` `webm` or `avi`. Any other key is
refused by name. A refusal ends the request with reason
`video:engine-bound`, or `size:engine-bound` for a size.

**[INF-SD7] Video result.** A job answer is read with a cap of 100 MiB. A
completed result whose format or `mime_type` differs from the request's
(`video/webm`, `video/x-msvideo`), whose `frame_count` or `fps` differs from
the values sent, whose decoded container exceeds 64 MiB, or whose leading
bytes are not EBML `1A 45 DF A3` or `RIFF…AVI ` is refused, and the engine's
error text is never read.

**[INF-SD8] First frame.** A caller's first frame reaches `sd-server` only as
the provider's own PNG: `sdcpp.FirstFrame` admits PNG or JPEG whose bytes
are the declared type, at most 16 MiB and 4096 on each side by its header,
crops its centre to the request's aspect, scales it to exactly the request's
width and height, flattens transparency onto black and encodes it again. Any
other frame is refused with reason `frame:engine-bound`.

## Video encoding

**[INF-VE1] Raw frames become H.264 in MP4 with the platform's encoder.** A
provider whose engine returns raw frames encodes them with the kit's
`providerhost/videoenc`, never with an encoder it bundles. Frames are RGB24
or RGBA rows, top to bottom, of one size with even sides from 32 to 4096,
at 1 to 240 fps; anything else is refused before an encoder starts
(`ErrOptions`). On Windows the kit drives Media Foundation's Sink Writer,
which ships with Windows, through OA's own COM binding: an MP4 container
(`MF_TRANSCODE_CONTAINERTYPE` MPEG-4) holding one H.264 High-profile
progressive video track at the frames' rate and size, at the caller's
average bit rate (at most 2^32 - 1 bit/s, the attribute's width) or 0.4
bits per pixel per frame and at least 1 Mbit/s, each frame lasting exactly
1/fps in 100 ns units. Writes are safe from several goroutines at once;
each frame takes the next index on the encoder's own thread, in arrival
order. The hardware H.264 encoder runs when the machine has one and the
caller did not ask for software; Microsoft's software encoder runs
otherwise, and the result names which ran (an encoder transform that names
a hardware URL is hardware). The MP4 is fast start: `moov` comes before
`mdat`, moved there by the kit with every chunk offset (`stco`, `co64`)
moved with the data, so a browser plays it while it downloads. When a
32-bit offset would pass 4 GiB once moved, every `stco` becomes a `co64` of
the same offsets; `moov` grows by 4 bytes per chunk, and every offset grows
by that final size. A fast start that fails after the encoder finished
never discards the video: the encoder's own file, `moov` last, is placed,
and the result reads `FastStart` false. The writer
writes a temporary name in the destination's folder, and Close moves the
finished file to the destination without replacing anything there
(`providerhost/noreplace`, the rename `models pack` uses); a destination
that already exists is refused at Create, and one that appears before
Close is refused then and left as it is. A video of no frame, a writer
that cannot finish and an aborted encoder leave no file of theirs. Every
other platform answers `ErrNoSystemEncoder`; no fallback encoder exists.
The delivery's media type is `video/mp4`.

## whisper.cpp

A provider that forwards transcription toward `whisper-server` enforces the
conditions of the whisper.cpp v1.8.4 security review
(`research/security-reviews/whisper.cpp-v1.8.4-win.md`) that concern its
options, its environment, the request and the answer. The shared provider
kit `go/providerhost/whispercpp` holds them once, with the WAV reader and
writer of `go/providerhost/wavfile`; the Lemonade provider uses them, and a
provider may refuse more on top.

**[INF-WC1] Options.** `whisper-server`'s command line is exactly
`-m <pinned model> --port <port>`. The provider never passes, and refuses
alone or as `--option=value`, `--host`, `--public`, `--request-path`,
`--inference-path`, `--convert`, `--tmp-dir`, `-vm`, `--vad-model`, `-oved`
and `--ov-e-device` (review condition 3).

**[INF-WC2] Environment.** `whisper-server` never receives a variable named
`WHISPER_*`, `OVERRIDE_VITISAI_*`, `KMP_*`, `GGML_*`, `HF_*`, `HTTP_PROXY`,
`HTTPS_PROXY`, `ALL_PROXY` or `NO_PROXY`, in any letter case (condition 4).

**[INF-WC3] The request.** The provider builds the multipart body itself.
`model` is `Whisper-Large-v3-Turbo`. `file`, named `audio.wav` of type
`audio/wav`, is the provider's own WAV of the caller's samples: the input
must be RIFF/WAVE with format tag 1, 16-bit, one or two channels at
8000..48000 Hz, a RIFF size equal to its length minus 8, one fmt chunk and
one data chunk of whole sample frames, at most 16 MiB; the provider writes
its own 44-byte header and the samples, so the bytes begin `RIFF` and a size
below 2^24. Every other input format is `unsupported_feature` with reason
`format:<media type>`, and malformed WAV ends the request `invalid` with
reason `audio:wav`. `language` is `auto` or
the primary subtag of the request's language in whisper's table, `prompt`
at most 1024 bytes of UTF-8 without NUL, `temperature` a finite number in
0..1, `response_format` one of `json`, `verbose_json`, `text`. No other
field reaches the engine (condition 7).

**[INF-WC4] The answer.** The provider reads at most 4 MiB with status 200,
of type `application/json` for `json` and `verbose_json` and `text/plain`
for `text`, and refuses anything else `unavailable` (condition 8).

## Kokoros

A provider that forwards speech toward Kokoros' `koko` enforces the
conditions of the Kokoros b17 security review
(`research/security-reviews/kokoros-b17-win.md`) that concern its command
line, its environment, the request and the answer. The shared provider kit
`go/providerhost/kokoro` holds them once; the Lemonade provider uses it.

**[INF-KO1] Command line.** `koko`'s command line is exactly
`-m <pinned model> -d <pinned voices> openai --ip 127.0.0.1 --port <port>`,
optionally with `--instances 1` or `2`; the `text`, `file` and `stream`
modes and any other address are refused (review condition 3).

**[INF-KO2] Environment.** `koko` never receives a variable named
`PIPER_*`, `ESPEAK*`, `ORT_*`, `SSL_CERT_*`, `HTTP_PROXY`, `HTTPS_PROXY`,
`ALL_PROXY` or `NO_PROXY`, in any letter case (condition 4).

**[INF-KO3] The request.** The provider builds the JSON body itself and
sends only `model` `kokoro-v1`, `input`, `voice`, `response_format` `pcm`,
`speed` and `stream` false. `input` holds 1..4096 characters of NFC text:
carriage returns become line feeds, tabs spaces, format characters and
variation selectors are dropped, and NUL, every other control character
except line feed, combining marks and characters NFC would change are
`unsupported_feature` with reason `text`. `voice` is one American or
British voice of the pinned voices file (`af_*`, `am_*`, `bf_*`, `bm_*`),
never a mix; another
voice is `unsupported_feature` with reason `voice:<name>`. `speed` is a
finite number in 0.5..2.0. The provider writes the WAV header of a `wav`
delivery itself; every format but `wav` and `pcm` is `unsupported_feature`
with reason `format:<word>`. It never sends `initial_silence`, `lang_code`,
`stream_format`, normalization, download or volume fields, or load options
(condition 6).

**[INF-KO4] The answer and a crash.** The provider reads at most 64 MiB
less the 44-byte header, with status 200, of type `audio/pcm` or `audio/l16`
at 24000 Hz, little-endian, one channel, and an even, non-zero byte count
(condition 7). A dropped connection or engine exit ends the request
`unavailable` with reason `engine:crashed`; the provider never resubmits
the text, names it in its log by SHA-256 only and admits the next speech
request only after a backoff (condition 8). The backoff refuses speech
alone. A provider whose engine process also serves other profiles stops it
only when no other model has admitted or running work, and its idle stop
(condition 9) likewise waits for work of every profile to end.

## The model name

`Request.model` is a family or alias string, as the router folds names. A
typed `abstraction.model` `Ref` would make every inference client depend on
the model, download request and storage content definitions. Hosted models
are named by their listing ids, which the router folds into families beside
local names. A later `model_ref` field can carry a `Ref` as a base change.

## Outcomes

| Call | Outcome | Meaning |
|---|---|---|
| `Start`, `Embed`, `Speech`, `Live.Append`/`Commit` (as `StartOutcome`) | `accepted` | Every admission step passed; the request runs (INF-A2, M1, M4). |
| `Start`, `Embed`, live and speech admission | `invalid` | Argument shape or a bound is wrong, or a required-extension key is listed but absent (INF-A2 step 1, A4). |
| `Start`, `Embed` | `no_host` | The router found no model server serving the request's model and modality (INF-A2 step 2, H2); reason `router:<verdict>`. |
| `Start`, `Embed` | `unsupported_feature` | The picked model server lacks a used feature, or (embed) speaks another wire (INF-A2 step 3, A4, M1). |
| `Start`, `Embed`, `Speech`, `Live`, the operator actions | `not_permitted` | The rights decision for `complete`, a credentials `Apply`, or a provider hook read anything but `permitted` (INF-A2 steps 4 and 8, A5). |
| `Start`, `Embed`, `Speech`, `Live`, `Transcribe` and the operator actions | `unavailable` | A decision point, a provider hook, the platform's credential store or an upstream provider could not answer; also a provider's own refusal under `lease:<outcome>`, `engine:none` or `object:unknown:<store>/<id>` (INF-A2). |
| `Start`, `Embed`, `Speech`, `Live` | `budget_exceeded` | A configured budget unit reached its daily count (INF-C2); reason `ceiling:<unit>:<credential>`. |
| `Start` | `exhausted` | The caller's concurrent requests are at the provider's bound (INF-A2 step 7). |
| `Start`, `Observe`, `Cancel` and the gateway | `forbidden` | The peer could not be bound (INF-A1), or a content grant was denied (INF-V1). |
| `Observe` (`PageOutcome`) | `gap` | `cursor` is older than the retained deltas; `next` names the oldest retained sequence (INF-Q2). |
| `Observe`, `Cancel` | `unknown` | No request of that id is visible to the caller (INF-Q1, Q4). |
| `Cancel` (`CancelOutcome`) | `cancelled` | This call stopped the request and closed its upstream call. |
| `Cancel` | `ended` | The request had already ended; `reply_outcome` says how (INF-Q4). |
| `Video.Observe` (`VideoState`) | `queued`, `generating` | The video runs; `queue_position`, or `step` of `steps`, when the engine reports it (INF-G6). |
| `Video.Observe` | `completed`, `failed`, `cancelled` | The video ended; only `completed` carries a delivery (INF-G3). |
| `Video.Observe` | `unknown` | No video of that id is visible, including one a provider or engine restart forgot; the job fails as `provider:lost` (INF-G7). |
| `Reply` (`ReplyOutcome`) | `completed` | The assistant message and `stop_reason` are present. |
| `Reply` | `refused` | The model server itself refused the request; reason `upstream:<status>`, or `upstream:context_length_exceeded:<window>:<input>` for a context-window refusal (INF-Q7). INF-W7 maps this to HTTP 502, and a context-window refusal to HTTP 400 with code `context_length_exceeded`. |
| `Reply` | `cancelled` | Reason `cancel`, `idle` (INF-Q3) or `caller`. |
| `Live.Append` | `duplicate` | A retry of the most recently acknowledged frame; no bytes sent upstream (INF-L2). |
| `Live.Append` | `out_of_order` | A repeated or skipped sequence other than the last acknowledged one (INF-L2). |
| `AddHost`, `RemoveHost`, `SetGateway` (`EditOutcome`) | `applied` | The declaration or setting changed; the reply carries the new revision. |
| `AddHost`, `RemoveHost`, `SetGateway` | `conflict` | `expected_revision` is stale, or `AddHost` named an existing model server. |
| `AddHost`, `RemoveHost` | `unknown` | `RemoveHost` named a model server no enabled declaration holds (INF-H3). |
| `IssueKey` (`EditOutcome`) | `conflict` | The program already holds an active key; revoke it first (INF-K2). |
| `IssueKey` | `no_secure_store` | The runtime has no platform credential store to hold the key. |
| `RevokeKey` | `unknown` | The program holds no active key. |
| `Hosts`, `Keys`, `Audit` (`ListOutcome`) | `page` | One page of the requested records. |

## Bounds

An image is at most the hosted server's own per-image limit, and a request's
total image payload is at most its total limit, both counting every
occurrence of a repeated digest (INF-V2). Embed accepts 1..64 texts of
1..32768 bytes, dimensions 1..4096, and returns at most 131072 float32
values in all (INF-M2). Transcription input is at most 16 MiB and segment
text at most 32768 bytes (INF-M5, M6). A live input frame is 1..65536 even
bytes; `Commit` needs at least 4800 bytes (100 ms) buffered (INF-L2, L3). A
gateway key is 32 random bytes (INF-K2). The audit journal retains its
newest 4096 entries across restarts (INF-P4), and `Audit`'s page is
1..256 entries. A request id is 1..128 bytes (`Admission.operation`).

## Divergences

- **INF-A2** decides features (step 3) and rights (step 4) before budget
(step 6). A caller with no rule on a model server never learns its budget
state. Several hosted gateways (LiteLLM, Portkey) decide a key's budget
before routing. OA routes and checks rights first: "no byte leaves the
service before every step passes" holds for spend as much as for content,
and a local `Pick` answering `no_host` is cheaper than a
spend check against a server the caller could never reach anyway.
- **INF-W8** serves one turn per Realtime connection; OpenAI's own Realtime
API keeps a session across turns. The gateway's one-turn shape matches
`live@1`'s own session model (INF-L1) instead of the upstream wire it
imitates.

## Not built

Automatic content transfer to a remote runtime, and gateway image uploads,
remain future work: INF-V3 requires the caller's own authorized store, and
the gateway currently refuses image input. No typed `abstraction.model`
`Ref` exists on `Request.model` (see "The model name"); a caller names a
family or alias string only. `IssueKey` mints a key for one program at a
time.

### Next: video generation

INF-G1, G3, G5, G6, G7, G8 and G9 bind ([Video generation](#video-generation)).
The rest of the first-run decision's video rules
(`research/first-run/ENGINES.md` §8) are not built:

- **INF-G2** (reserved). A model server of kind `graph` registers with its
base URL and a template directory; each template whose files are present
is a servable family in the router (router CONTRACT.md ROUTE-H2, held in
the placement store).
- **INF-G4** (reserved). Rights action `abstraction.inference/generate` on
`server:<name>`; spend counts `seconds` for video and `images` for image.
Until then a video job decides `abstraction.inference/complete`, and a
hosted credential's ceiling counts one image per video.
- A delivery `path` where the caller's placement is this machine (the
second half of the reserved INF-G3). `Delivery` is released and closed.
- `fps` and `seed` from the caller, until a job document version adds them.
`video@1` carries both, and the runtime sends zero (the preset's rate, the
provider's seed) because the released, closed job `Request` has no field for
them.
- The runtime's declaration of a video provider (`openabstractions models
host` writes chat and embed profiles only). The modelhost provider serves
`video@1` with `sd-server` behind it (INF-SD4..INF-SD8).
