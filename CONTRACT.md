# abstraction.inference contract

`inference.thrift` defines `abstraction.inference/chat@1`: model calls the
receiving runtime performs for a bound caller, and `operator@1`: the runtime's
hosts, the gateway window's local keys and the inference audit. The decision is
`research/inference/DECISION.md` (VISION 2026-09-16, "Hosted model calls are an
inference seat"). This file states the obligations a provider meets.

## Admission

- **INF-S1.** Every call binds the receiving peer by native Program proof
  (kernel user and process, bound path). A peer that cannot be bound, or a peer
  of another account, reads `forbidden` before the request is examined further.
- **INF-S2.** `Start` evaluates in this order and stops at the first refusal:
  1. argument shape and bounds (`invalid`);
  2. guarantees and host selection through the router (`no_host`, reason
     `router:<verdict>`);
  3. the chosen host's features and required extensions
     (`unsupported_feature`, reason `feature:<name>` or `extension:<key>`);
  4. the rights decision for `abstraction.inference/complete` on
     `host:<name>` for the bound subject (`not_permitted`, reason
     `rights:<word>`; `unavailable`, reason `rights:unavailable`, when the
     decision point cannot answer);
  5. image content resolution under the original caller's rights (INF-V1);
  6. the credential's ceiling (`budget_exceeded`, reason
     `ceiling:<unit>:<credential>`);
  7. the caller's concurrent operations (`exhausted`, reason `capacity`);
  8. for a hosted host, `abstraction.credentials/applier@1` `Apply` with
     consumer `abstraction.inference/chat@1`, the bound subject, the host's
     credential name and the host's lowercase host name (`not_permitted` with
     reason `credential:<outcome>:<name>`; `unavailable` with reason
     `credential:unavailable:<name>`).

  No byte leaves the service before every step passes. A decision point that
  cannot answer never reads as permission.
- **INF-S3.** Guarantees select hosts. A request without
  `abstraction.inference/hosted-allowed@1` is served only by hosts on the
  receiving machine. A request with it may also be served by a hosted host
  whose credential name equals `Request.credential`, ranked after every
  resident and would-load local host (router `Pick`). A request naming both
  `local-only@1` and `hosted-allowed@1`, or an unknown guarantee, is `invalid`.
- **INF-S4.** A host lacking a feature the request uses refuses it before
  spend: `tools` needs `abstraction.inference/tools@1`, `options.json_schema`
  needs `abstraction.inference/json-schema@1`, and an `image` part needs
  `abstraction.inference/vision@1` and a content reader. A key in
  `required_extensions` the host does not know is refused with reason
  `extension:<key>`, and a key listed there but absent from `extensions` is
  `invalid`.

## Image input

- **INF-V1.** `vision@1` accepts image parts in user messages. Each names a
  canonical lowercase `sha256:` digest and one of `image/png`, `image/jpeg`,
  `image/gif` or `image/webp`. The receiving service reads content under the
  original bound caller's authority after the host's `complete` decision.
  Storage checks permission before lookup and while reading. A digest grants
  no permission. Authorized missing content is `invalid` with
  `content:unknown`; denied access is `forbidden` with `content:forbidden`;
  an unavailable reader is `unavailable` with `content:unavailable`.
- **INF-V2.** Before applying credentials or starting the upstream call, the
  service verifies the complete bytes against the digest and checks the
  detected media type. A mismatch is `invalid` with `content:digest` or
  `content:media-type`. Provider limits bound each image and the total image
  payload, counting every occurrence even when a digest is resolved once.
  Exceeding either limit is `invalid` with `content:too-large`. Text and image
  order is preserved in the upstream request, including image-only messages.
- **INF-V3.** OA calls carry digest references. The upstream adapter encodes
  bytes into its vendor format. A remote OA runtime receives the original
  references and resolves them in its own authorized store; delegation does
  not transfer content. Its admission refusal ends the delegated operation
  with the existing typed remote refusal.

## Credentials and spend

- **INF-C1.** The service applies a hosted host's credential per outgoing
  request: the first request of an operation and every retry or reconnect call
  `Apply` again. Applied headers are sent once and appear in no reply, delta,
  log record, error or state file.
- **INF-C2.** A ceiling belongs to a credential name: tokens per UTC day,
  spend in currency millionths per UTC day, or both. A ceiling record also
  carries requests, images, audio seconds and characters per UTC day for the
  profiles that count them. This version enforces requests, counting one per
  ended chat operation and one per embed call, and stores and lists images,
  audio seconds and characters without enforcing them. The provider counts usage
  and cost from provider replies, keeps the counts across its own restart, and
  refuses `budget_exceeded` at admission once a count has reached its ceiling.
  An operation admitted under the ceiling runs to its end; its reply counts.
- **INF-C3.** One log record per ended operation and per admission refusal:
  program, host, model, family, tokens in, tokens out, wall time, outcome,
  reason, credential name and ceiling state. Records carry names, never
  header values or message content.

## Embeddings

`embed@1` is one bounded call, decided in `research/inference-modalities/DECISION.md`.

- **INF-E1.** `Embed` admits in `Start`'s order for profile `embed`, without the
  feature and capacity steps. The steps are argument shape, host selection, the
  host's wire, the `complete` decision on `host:<name>`, the credential's
  ceiling, and for a hosted host `Apply` with consumer
  `abstraction.inference/embed@1`. No byte leaves the service before every step
  passes. A credential scoped only to `chat@1` reads
  `not_permitted`, reason `credential:<outcome>:<name>`.
- **INF-E2.** A request carries 1..64 texts of 1..32768 bytes and, optionally,
  dimensions 1..4096. A completed reply carries one vector per input in input
  order, each the base64 of `dimensions` little-endian float32 values, at most
  131072 values in all. The provider accepts an upstream's number arrays and
  base64 float32 alike and returns the same bytes for both. An upstream reply
  of another count, length or requested dimension reads `unavailable`.
- **INF-E3.** Every call leaves one audit record with `profile` `embed`, the
  host, model, input tokens, wall time, outcome and reason, and never the
  texts or vectors. The gateway window serves `POST /v1/embeddings`, returning
  number arrays or, for `encoding_format` `base64`, the vectors as sent.
  A remote host receives the request unchanged except for the model and the
  claim extension.

## Transcription

`transcription@1` accepts one canonical storage digest and returns ordered,
timestamped segment or word deltas through the shared operation lifecycle.

- **INF-T1.** Admission validates the request, selects a host that explicitly
  serves profile `transcription`, checks `complete` on `host:<name>` for the
  original bound caller, resolves the digest under that same caller's content
  policy, checks ceilings and capacity, and only then applies a credential and
  sends bytes upstream. Unknown content is `invalid`, another caller's content
  is `forbidden`, and an unavailable store is `unavailable`. None spends an
  upstream request.
- **INF-T2.** Input is at most 16 MiB, its bytes must match its SHA-256 digest
  and declared WAV, MP3, MP4, WebM, Ogg or FLAC media type. whisper.cpp accepts
  WAV only. References cross OA and remote wires; bytes exist only in the
  selected backend request, and a remote runtime resolves the digest in its
  own authorized store.
- **INF-T3.** Segment text is at most 32768 bytes. Deltas preserve upstream
  order and millisecond timestamps; malformed timestamps or oversized text
  end `unavailable`. Idle, caller and explicit cancellation close local or
  delegated upstream work. Audit and ceilings count the terminal duration in
  rounded-up audio seconds under profile `transcription`.

## Operations

- **INF-O1.** An operation is identified by its opaque `operation` id within
  the provider epoch, and is visible only to the account and program that
  started it. Any other caller reads `unknown` for `Observe` and `Cancel`.
- **INF-O2.** Deltas are numbered from 0 without gaps, and the last is
  `end`. `Observe` returns deltas from `cursor`; the newest
  `Admission.retained_deltas` deltas are readable, and an older cursor reads
  `gap` with `next` at the oldest retained sequence. `max_bytes` bounds the
  compact JSON encoding of the returned deltas. `at_end` is true when the page
  holds the end delta or `cursor` is already past it.
- **INF-O3.** An operation with no `Observe` in flight and none received for
  `Admission.idle_ms` is cancelled with reason `idle`, and its upstream request
  is closed within one second after that. A caller that exits therefore stops
  spend within `idle_ms` plus one second. A caller that disconnects in the
  middle of an `Observe` stops that wait at once.
- **INF-O4.** An ended operation stays observable for `Admission.retention_ms`,
  then reads `unknown`. `Cancel` of an ended operation reads `ended` with its
  reply outcome and changes nothing.
- **INF-O5.** A retry of `Start` is a new operation. The contract offers no
  request identity for `Start`: a lost `Admission` leaves an operation that
  idles out within `idle_ms` unobserved, at most one request's spend.

## Clients

- **INF-K1.** Each language's client offers `complete()`, which starts an
  operation, observes it to its end and folds the deltas into a `Reply`, and
  `stream()`, which yields deltas in the language's native iteration form and
  cancels the operation when the iteration is abandoned. A `Start` refusal is a
  `Reply` with that outcome and no deltas. Clients never retry `Start`.

## The gateway window

The window is the runtime's loopback surface for programs that only know a
base URL and an API key: OpenAI `POST /v1/chat/completions` and `GET
/v1/models`, and Anthropic `POST /v1/messages`, with server-sent events on
both. It is the same provider, rights decision and audit as the native route.

- **INF-W1.** The window listens on IPv4 loopback only, and it is closed by
  default. It opens from the gateway setting in the runtime state
  (`inference/gateway.json`), which operator@1 `SetGateway` writes and applies
  to the running runtime, or for one foreground runtime from `--gateway
  127.0.0.1:<port>`, which leaves the setting unchanged. Closing it stops the
  listener and every window connection before `SetGateway` replies, and a
  runtime restart reopens it from the setting.
- **INF-W2.** Each accepted connection's peer is bound through the socket-owner
  table (identity `BindLoopback`, ID-T1..T6) before a byte is read, and must
  meet user, process and path at `bound` as an account of the runtime. A peer
  that cannot be bound is answered 403 and the connection closed with its
  request unread. On macOS every connection is refused this way.
- **INF-W3.** A request's binding is rechecked after its head is read and again
  after its body, before admission. A connection that no longer answers for the
  program that opened it is refused with reason `binding:peer_moved`.
- **INF-W4.** The key (`Authorization: Bearer` or `x-api-key`) is verified
  against the holder record of kind `openabstractions/local-key@1` in constant
  time before the body is read. A missing, unknown or revoked key is 401; a key
  issued to another program than the bound one is 403 with reason
  `key:wrong_program`. Refusals set `Connection: close` and read no body.
- **INF-W5.** A key grants nothing by itself. A request is admitted through
  `Start` for the bound subject, so `abstraction.inference/complete` on the
  host and, for a hosted host, `abstraction.credentials/apply` decide it. A
  key issued with a credential name adds `hosted-allowed@1` and that name to
  every request; a key without one adds `local-only@1`.
- **INF-W6.** Every window refusal before admission, and every admission
  refusal and ended operation of the window, is one audit entry with route `window` and the
  rung of the bound peer (`tcp-loopback/<platform> user=<proof> process=<proof>
  path=<proof>`, or `... unbound`). Native entries carry route `native` and the
  pipe or socket rung.
- **INF-W7.** Start refusals map to HTTP statuses: `invalid` and
  `unsupported_feature` 400, `not_permitted` and `forbidden` 403, `no_host`
  404, `budget_exceeded` and `exhausted` 429, `refused` 502, others 503. The
  error body carries `type`, and `error` with `type`, `message` and `code`
  (the outcome word), readable by both wires. A reply that ends other than
  `completed` in a stream is an error event, then the stream's end. A request
  field chat@1 cannot express (`n` other than 1, an image part, an unknown
  response format) is `unsupported_feature` before admission. A client that
  disconnects cancels its operation.

## The operator profile

- **INF-P1.** `Hosts`, `AddHost` and `RemoveHost` are decided on
  `abstraction.inference/host.manage`, `Keys`, `IssueKey` and `RevokeKey` on
  `key.issue`, and `Audit` on `audit.read`, each on resource `account` for
  the bound caller. The default installation grants them to the runtime's
  operator programs.
- **INF-P2.** Host edits are conditional on the configuration revision.
  `AddHost` writes the permit rule `complete` on `host:<name>` for the
  operator programs and the caller, and for a hosted host with a credential the
  runtime program's `abstraction.credentials/apply` on `credential:<name>`,
  with why `inference host add`; an existing rule on a target is left as it
  is. The router reads the new hosts at once. The stored host records its
  `profiles` (the wire's default when the request names none: every seeded
  profile for `openai-compatible` and the local kinds, `chat` otherwise) and
  `declared_by` `operator`; a request that names `declared_by` is `invalid`.
  The router picks by model and profile (INF-H2).
- **INF-P3.** `IssueKey` mints 32 random bytes, holds them as a holder record
  of kind `openabstractions/local-key@1` scoped to consumer
  `abstraction.inference/chat@1`, and returns the key once. The holder never
  applies that kind to an outgoing request, and holder@1 `Store` refuses it.
  A program holds at most one active key.
- **INF-P4.** The audit journal keeps the newest 4096 entries across runtime
  restarts, numbered without gaps. Entries carry names and counts, never a
  header value, key or message content.
- **INF-P5.** `Gateway` and `SetGateway` are decided on `host.manage` on
  resource `account`. `SetGateway` is conditional on the setting revision, the
  digest of the setting file, and refuses an address other than
  `127.0.0.1:<port>` as `invalid` with reason `address`. A setting written
  whose window cannot listen reads `unavailable` with reason `listen:<detail>`,
  and `Gateway` reports the same reason in `why`.

## Host registration

The runtime's hosts come from three sources
(research/inference-registration/DECISION.md): the operator's `hosts.json`,
the products' own declarations, and the declarations of
`abstraction.facade/registry@1`.

- **INF-H1.** A product declaration is read from the product's own record of
  where it listens, and nothing else: Ollama's `OLLAMA_HOST`, parsed as
  Ollama's client parses it; LM Studio's `http-server-config.json` under
  `~/.lmstudio/.internal` or `~/.cache/lm-studio/.internal`; Docker Desktop's
  `settings-store.json` with `EnableInferenceTCP` true, or Docker Engine's
  `docker-model` CLI plugin on Linux, both at the documented port 12434; and
  the address `foundry service status` prints for Foundry Local, which picks a
  port at each start and writes none to a file. A missing record is the
  product's documented default for Ollama and LM Studio and no host for Docker
  Model Runner and Foundry Local. A malformed record is the default with a
  logged reason. No probe opens a connection, scans a port or listens for
  multicast. llama-swap records its listen address only in its command line
  and has no default configuration path, and an operator names it. A declared
  host lists `declared_by` as the product's name; Lemonade and ComfyUI, which
  record no address, list `default`. `hosts.json` `declared` switches
  declarations off; removing a declared host through `RemoveHost` does so and
  keeps the other hosts as operator entries.
- **INF-H2.** Every host carries profiles: those its registration declares, or
  its wire's default. A local product's models carry their own profiles from
  its metadata where it reports them: LM Studio `type` `llm` and `vlm` serve
  `chat` and `embeddings` serves `embed`; Ollama `/api/show` capabilities
  `completion` serves `chat`, `embedding` `embed` and `image` `image`. router@1
  `Pick` takes the profile beside the model and picks only a name its host
  serves for it; with no servable host serving the profile, or only hosts
  that serve the model for other profiles, the verdict is `no-host`. chat@1
  picks with profile `chat` and refuses such a request `no_host` with reason
  `router:no-host`.
- **INF-H3 to INF-H5** moved to the facade registry
  ([facade CONTRACT.md](https://github.com/openabstractions/abstraction-facade/blob/main/CONTRACT.md)): provider
  declarations are REG-1 and REG-2, inventory-source acceptance REG-3, and a
  remote runtime's declaration REG-4. Its delegation stays here, under
  "Remote runtimes".

## Remote runtimes

`chat@1` names no base URL, header, key, endpoint or local path. A request
carries a credential *name*, the operation id is opaque, and `Reply.host` is a
display name. A local runtime can therefore delegate a request unchanged to
another runtime holding the credential through the mutual-TLS remote transport:
the remote binds its own client certificate subject, applies the name from its
own holder under its own rights, and the local service relays deltas and
records the delegation as an attributed claim. No applied header or local pipe
proof crosses the network.

A remote runtime is declared in `abstraction.facade/registry@1` (facade REG-4).
A chat@1 operation the router picks for it is delegated: the request goes
unchanged except for the chosen model name and the extension
`openabstractions/claimed-program` set to the bound program, its deltas are
relayed, and cancelling it cancels the remote operation. Local ceilings and the
local applier are not used. The receiving runtime maps the client certificate
key to its caller, decides and applies for the subject {mapped namespace, its
own executable}, and records route `remote`, `domain` the namespace and `claim`
the claimed program. The delegating runtime records `domain` the remote host.
The receiving runtime answers endpoint@1 `Describe` over the remote transport
with chat@1 and router@1.

The window is a loopback surface of one machine and never a remote one: a peer
on another machine has no socket-owner record here, and INF-W1 keeps the
listener on 127.0.0.1. A remote operator reaches operator@1 the way it reaches
chat@1, through the remote transport, where the receiving runtime maps the
client certificate to its own subject and decides the operator actions for it.
A local key minted on one runtime means nothing to another.

## Rule answers

The ten rules of "Writing a contract", and rule 11, answered before generation.

1. **Outcomes.** `Start` returns `StartOutcome`, `Observe` `PageOutcome` and
   `Cancel` `CancelOutcome`; the operation's result is `ReplyOutcome`. All
   reserve `forbidden`, `unavailable` and `invalid`. `Observe` and `Cancel`
   address an operation and reserve `unknown`. No chat@1 call is a conditional
   write. An unreachable rights decision point or holder is `unavailable`
   (INF-S2). operator@1 lists reserve `invalid`, `forbidden` and
   `unavailable`; its edits add `conflict` for a stale configuration revision
   or an active key, `unknown` for an absent host or key, and
   `no_secure_store` for a key with nowhere to be held.
2. **Retained records.** An operation is identified by its id within the
   provider epoch (INF-O1); `Start` has no retry dimension, and a retry is a new
   operation (INF-O5). Retention is readable in `Admission.retention_ms` and
   `retained_deltas`. Loss of older deltas is the typed `gap`; loss of the
   operation after retention or restart is `unknown`. The starting caller
   retires it by `Cancel` or by leaving it idle; a replay after retirement
   reads `unknown`. The ceiling counts are provider state, not a caller record.
   A local key is a holder record (credentials CONTRACT retention and
   tombstones) named `local-key.<program tag>.<random>`, so a reissued key
   never collides with a revoked one's tombstone. Audit entries are identified
   by sequence; entries older than the retained journal read `gap`.
3. **Failures.** `ReplyOutcome` is the class, and `reason` carries the typed
   cause as `<source>:<word>` from a fixed set of sources (`rights`,
   `credential`, `ceiling`, `feature`, `extension`, `router`, `upstream`) plus
   `cancel`, `idle` and `caller`. No cause enum is declared; a later cause
   enum follows rule 3.
4. **Catalogues.** `RequestGuarantee` and `features` are open seeds; a code
   provider declares further features by `<owner>/<name>@<n>`, and a request
   extension uses `extensions` keys `<owner>/<name>`. `resource_actions` is
   closed by INF-R1: the four actions are this capability's own, registered
   into the host's rights decision policy; the next name is
   `abstraction.inference/budget.manage`. `credential_consumers` names this
   profile's applier consumer. The router's `wire_kinds` is the open wire
   catalogue. `local_key_kinds` names the one holder kind the window verifies,
   and `local_host_kinds` the local runtimes AddHost composes; both grow with
   the runtime that serves them. `host_profiles` is open: a later profile is a
   seed member or `<owner>/<name>@<n>`. `host_declarers` seeds `operator`,
   `default` and the four products INF-H1 reads; a product probe added later
   adds its own name.
5. **Identity fields.** The service binds the caller from native Program proof
   and asserts it as `Use.subject` to the applier and in log records. The
   request carries no identity claim. `Reply.host` and `Reply.model` are
   asserted by the service from its router decision; an empty host means
   nothing served the request.
6. **Bounds.** A request fits one 1 MiB control frame: messages 1..512, parts
   1..64 per message, tools 0..128, stop 0..8. Images cross by storage digest.
   A page is at most 256 deltas and 65536 bytes of encoded deltas. operator@1
   lists at most 64 hosts and 256 keys, and an audit page at most 256 entries.
   The window reads a request body of at most 1 MiB.
7. **Unknown members.** Every enum is acted on and refuses unknown members.
8. **Entry points.** INF-S, INF-C and INF-O rules judge the `chat@1` service;
   INF-K1 judges each language's client; INF-W rules judge the gateway window and
   INF-P rules the operator profile.
9. **Duplicate keys.** Refused.
10. **First definition.** Recorded as a "none" entry in
    `docs/BASE-PROTOCOL-CHANGES.md`; rules 1 to 6 were answered here before
    generation.
11. **Reserved words.** No field, method or parameter name maps to a reserved
    word in Go, C++, Python, Rust or JavaScript. `Delta.end` is a Ruby keyword
    only, and Ruby is not generated.

- **INF-R1.** `resource_actions` is closed: `complete` on `host:<name>`,
  `host.manage`, `key.issue` and `audit.read` on `account`. The default
  installation grants none of them to applications.

## The model name

`Request.model` is a family or alias string, as the router folds names. A typed
`abstraction.model` `Ref` would make every inference client depend on the model,
download request and storage content definitions. Hosted models are named by
their listing ids, which the router folds into families beside local names. A
later `model_ref` field can carry a `Ref` as a base change.

# Speech and live generated output

The `speech@1` and `live@1` profiles use the shared operation lifecycle and
original-caller authorization. Speech streams bounded audio chunks and returns
its complete bytes through a typed `Delivery`. Live accepts ordered PCM input,
streams transcript/audio deltas, and commits one final response.

Generated output is authorized before paid work with
`abstraction.storage/content.write` on `output:speech` or `output:live`.
The runtime binds that grant to the original subject, media type and byte limit,
and rechecks the same grant at publication. Digest-addressed uploads keep their
existing digest policy. Result reads require the content reader's authorization.
Once result publication starts, cancellation waits for that publication's outcome.

Live `Append` uses mono signed 16-bit little-endian PCM at 24000 Hz, in even-sized
frames of 1..65536 bytes. Retrying the most recently acknowledged identical frame
returns `duplicate` and sends no bytes upstream. Other repeated or skipped
sequences return `out_of_order`. An uncertain send terminates the session;
accounting includes the attempted frame. `Commit` needs at least 100 ms of input
(4800 bytes), closes input and requests one final response. Repeated Commit is
idempotent. Observe retains its cursor/replay semantics after Commit.

The OpenAI Realtime wire is explicitly selected; ordinary OpenAI-compatible
hosts do not acquire live support automatically. The native service and Go
client support live sessions. Service-to-service forwarding uses the authenticated
remote frame transport and attributes final deliveries to the remote trust domain.
Remote credentials, output authorization and accounting remain owned by that
runtime.

The optional `/v1/realtime` WebSocket window supports one turn: `session.update`
selects PCM16 at 24000 Hz with manual turn detection; `input_audio_buffer.append`
adds audio; `input_audio_buffer.commit` acknowledges input closure; and
`response.create` invokes native Commit. Unsupported session options and events
are refused. Local key and peer authority are rechecked during the session.
Disconnect, revocation and window shutdown cancel unfinished operations.
Loopback protocol fixtures prove this event mapping; real-provider interoperability
has not been measured.
