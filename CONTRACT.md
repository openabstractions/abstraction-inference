# abstraction.inference contract

`inference.thrift` defines seven services: `abstraction.inference/chat@1`,
`embed@1`, `transcription@1`, `speech@1`, `live@1` and `image@1` — model calls
the receiving runtime performs for a bound caller — and `operator@1`, which
administers the runtime's model servers, the gateway and its gateway keys,
and reads the inference audit. `job.thrift` defines the durable document job
kind `inference` carries for video and image-batch work, executed by the same
provider. This file states the obligations a provider meets.

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
| G | reserved for video generation, not built this release ([Next](#next-video-generation)) |

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
`extensions` is `invalid`.

**[INF-A5] Provider-side admission hooks.** A provider whose own program
has work to do on the picked model server before a call is decided
declares `Config.Admit`, run for `Start` and `Embed` after step 3 and
before the rights decision (step 4). A false answer refuses the request
`unavailable` with the hook's own reason word, or `not_permitted` when
that word is exactly `not_permitted`. A nil hook changes nothing; the
model host declares one to load its object before the route and keep its
own refusal words (`abstraction-provider-modelhost` CONTRACT.md MODELHOST-H2).

## Image input

**[INF-V1] Digest references.** `vision@1` accepts image parts in user
messages. Each names a canonical lowercase `sha256:` digest and one of
`image/png`, `image/jpeg`, `image/gif` or `image/webp`. The receiving
service reads content under the original bound caller's authority after
the model server's `complete` decision. Storage checks permission before
lookup and while reading. A digest grants no permission, and a grant for
one content action grants nothing under the other: a write grant that
stored the bytes does not authorize reading them back. Authorized missing
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
`unsupported_feature`, reason `wire:<name>`.

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

## Speech

**[INF-S6] Speech delivery.** `speech@1` streams bounded audio chunks and
returns its complete bytes through a typed `Delivery`, using the shared
request lifecycle and original-caller authorization.

**[INF-S7] Output authorization.** Generated output (`speech@1`, `live@1`)
is authorized before paid work with `abstraction.storage/content.write` on
`output:speech` or `output:live`. The runtime binds that grant to the
original subject, media type and byte limit, and rechecks the same grant
at publication. Digest-addressed uploads keep their existing digest
policy. Result reads require the content reader's authorization. Once
result publication starts, cancellation waits for that publication's
outcome.

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
account of the runtime. A peer that cannot be bound is answered 403 and
the connection closed with its request unread. On macOS every connection
is refused this way.

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
`message` and `code` (the outcome word), readable by both wires. A reply
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
`server:<name>`, `host.manage`, `key.issue` and `audit.read` on `account`.
The default installation grants none of them to applications. The next
name this catalogue would take is `abstraction.inference/budget.manage`.

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
| `Reply` (`ReplyOutcome`) | `completed` | The assistant message and `stop_reason` are present. |
| `Reply` | `refused` | The model server itself refused the request; reason `upstream:<status>` (INF-W7 maps this to HTTP 502). |
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

`research/first-run/ENGINES.md` §8 asks 0.3.0 to carry a graph-kind model
server so ComfyUI, and later a dedicated engine, can serve video and image
generation as durable job-kind `inference` work
(`research/inference-modalities/DECISION.md`, 2026-09-17: video is not a
modality, it is job kind `inference` carrying an `image@1`-shaped request
with `duration`). The four rules below carry the ids the first-run decision
settled on, `INF-G1` to `INF-G4` (letter G, generation); vision keeps letter
V. They are not binding this release; they let 0.4.0 register a provider
without a contract shape change.

- **INF-G1** (reserved). A `generate` request names `server`, `template` or
`model`, `prompt`, `size`, `duration_s`, `preset`; with `duration_s` it is
accepted only as job kind `inference`.
- **INF-G2** (reserved). A model server of kind `graph` registers with its
base URL and a template directory; each template whose files are present
is a servable family in the router (router CONTRACT.md ROUTE-H2, held in
the placement store).
- **INF-G3** (reserved). A generation result is a typed reference committed
to the content store; where the caller's placement is this machine, the
delivery carries `path`.
- **INF-G4** (reserved). Rights action `abstraction.inference/generate` on
`server:<name>`; spend counts `seconds` for video and `images` for image.
