# abstraction-inference

Call text, embedding, speech, transcription, image or live-voice models without
putting a provider key or vendor protocol in the application. The application
names a model family and the behavior it needs. The runtime chooses an allowed
local engine such as Ollama or Lemonade, or a hosted provider such as
OpenRouter, then applies the named credential inside the service.

[CONTRACT.md](CONTRACT.md) states the obligations. `inference.thrift` is the
definition. The generated Go, C++, Python, Rust and JavaScript protocol code
lives under `go/`, `cpp/`, `py/`, `rs/` and `javascript/`.

The request carries messages of typed parts (text, image by storage digest,
tool call, tool result), tools, options, vendor extensions, guarantees and a
credential name. A reply streams as bounded pages: `Start` admits the call,
`Observe` reads deltas from a cursor with a long-poll wait, and `Cancel` stops
it. An operation nobody observes is closed, because a stream nobody reads is
spend.

## Choose an entrypoint

| Application | Entry point | Identity and policy |
| --- | --- | --- |
| OA client | Resolve the generated profile through the facade | Native caller program is checked on every call |
| OpenCode | [OA JavaScript provider](https://github.com/openabstractions/abstractions/tree/main/adopters/opencode) | OpenCode executable is the native caller |
| OpenAI/Anthropic-compatible client | Optional loopback gateway window | Issued key is bound to one program; rights still decide |
| MCP client | [Curated MCP gateway](https://github.com/openabstractions/abstractions/tree/main/mcp-gateway) | Gateway executable is an explicit integration principal |

For a local OpenAI-compatible runtime, declare the host and grant one
application access:

```console
openabstractions inference host add ollama --base http://127.0.0.1:11434
openabstractions rights grant --for inference --program /absolute/path/to/app --host ollama
```

For a hosted provider, register the credential value through standard input,
declare the host using its name, then grant the application:

```console
openabstractions credentials add openrouter --target openrouter.ai \
  --for abstraction.inference/chat@1 --for abstraction.router/router@1 \
  --use-by /absolute/path/to/app --from-stdin
openabstractions inference host add openrouter \
  --base https://openrouter.ai/api/v1 --wire openai-compatible \
  --credential openrouter --tokens-per-day 200000
openabstractions rights grant --for inference --program /absolute/path/to/app \
  --host openrouter --credential openrouter
```

The provider configuration and inference request retain `openrouter`, the
credential name. Holder and Applier keep the value at the service boundary.
Use `openabstractions inference audit` and `openabstractions credentials audit`
to inspect attributed decisions. These commands require an authorized operator
and a running runtime.

## Capabilities

The current source provides these profiles through the runtime:

| Profile | Application receives |
| --- | --- |
| `chat@1` | Text and tool-call deltas, including vision input by digest |
| `embed@1` | Bounded embedding vectors |
| `transcription@1` | Text and timestamped segments from stored audio |
| `speech@1` | Streamed audio and a digest-addressed final delivery |
| `image@1` | Generation or edit progress and typed image deliveries |
| `live@1` | One voice turn with ordered PCM input, transcript and audio output |

The Thrift definition generates API types and protocol bindings for Go, C++,
Python, Rust and JavaScript. The Go service owns provider communication,
credentials and output storage. The [contract](CONTRACT.md) describes lifecycle
and authorization; generated bindings alone do not establish tested SDK coverage.

## Go provider

- `inference.Provider` admits a request in the contract's order: shape,
  host selection through `router.Router` within the request's guarantees,
  features, the `abstraction.inference/complete` decision on `host:<name>`,
  authorized image reads, the credential's ceiling, capacity, then `Apply` for a hosted host. It runs
  the upstream request, keeps each operation's newest deltas, cancels an
  operation nobody observes, and retires ended ones after retention.
- Wires: `openai-compatible` (OpenRouter, OpenAI, Groq, LiteLLM, and the `/v1`
  of Ollama, LM Studio and Lemonade) and `anthropic-messages`, both streamed as
  server-sent events.
- `inference.Ceilings` counts tokens and reported cost per credential per UTC
  day in an explicit state file.
- `service.Listen` serves the inference profiles on one endpoint and binds every caller by
  Program proof. On macOS every call reads `forbidden` until identity proof
  lands.

The same endpoint serves `transcription@1`. A request names an audio digest in
the runtime content store; the provider authorizes and verifies at most 16 MiB
for the original caller, then sends bytes to an OpenAI-compatible or Lemonade
transcription endpoint, whisper.cpp `/inference`, or Deepgram prerecorded
endpoint. Timestamp units are returned as retained segment deltas, with the
same cursor, cancellation, ceiling, credential and audit behavior as chat.

## Sending images to chat

An application uploads an image through the runtime's storage content writer,
then puts its digest and media type in a user message. The runtime reads the
image, verifies its digest, and sends it to the selected model. PNG, JPEG, GIF
and WebP inputs are supported by the OpenAI-compatible and Anthropic adapters.
The model must support image input.

The application needs `abstraction.storage/content.write` and
`abstraction.storage/content.read` rights for that digest, plus the host's
`complete` right. The user runtime serves the reader and writer over one
service-owned store, with a 16 MiB limit per image and a 64 MiB total image
payload limit per request. Repeated images count toward that total.

A remote runtime needs the content in its own authorized store. Automatic
content transfer and gateway image uploads remain future work; the gateway
currently refuses image input. Native OA clients use digest references.

## Generated images, speech and live voice

Image generation and speech return typed deliveries containing a digest, media
format, size and storage location. Applications read the bytes through the
content service. Image editing names uploaded image and mask digests. The service
authorizes inputs and output publication for the original calling program.

Live voice accepts mono PCM16 at 24 kHz through `Append`, followed by `Commit`
for one response. The client observes transcript and audio deltas. Retrying the
last acknowledged identical input frame is safe. The contract defines refusal
and cancellation behavior when a transport outcome is uncertain.

Optional compatibility routes include `/v1/images/generations`,
`/v1/images/edits`, `/v1/audio/speech` and `/v1/realtime`. The Realtime window
supports one manually committed audio turn. Its supported event subset is
specified in the contract. Loopback tests cover that mapping; live upstream
interoperability remains unmeasured.

## Durable video and image batches

Applications submit a generated `job.thrift` request document as job kind
`inference` through the existing job API. They keep the receipt to observe,
reconcile, cancel and read the typed result after their process exits. The runtime
uses the same caller permissions, credential holder and content storage as other
inference calls. Replicate asynchronous predictions are the first backend.

The required `abstraction.inference/recoverable-upstream@1` guarantee records
submission intent and provider handles durably. A known handle resumes after
restart. A lost create reply remains visibly `upstream:uncertain` when the backend
cannot look up the caller's operation; the runtime preserves that uncertainty
without submitting duplicate work. Result documents contain authorized content
references. The job contract retains its provider-independent vocabulary.

## Runtime configuration

The user runtime (`openabstractions serve runtime`) composes the provider with
its rights policy and credentials holder. Hosts come from
`<state>/inference/hosts.json`:

```json
{
  "local": [{"kind": "ollama", "base": "http://127.0.0.1:11434"}],
  "hosted": [{"name": "openrouter", "base": "https://openrouter.ai/api/v1",
              "wire": "openai-compatible", "credential": "openrouter"}],
  "ceilings": {"openrouter": {"tokens_per_day": 200000, "micros_per_day": 5000000}}
}
```

Without the file, or without `local`, the runtime reaches this machine's
Lemonade, LM Studio and Ollama at their default addresses. A hosted host's
listing is read as the runtime's own program, so its credential needs an
`apply` rule for that program and consumer `abstraction.router/router@1`; each
application needs its own `apply` rule and an `abstraction.inference/complete`
rule on `host:<name>`.

`abstraction.inference/operator@1`, served on the same endpoint, edits that
file and reads the audit. `openabstractions inference host add|list|remove`,
`key issue|revoke|list` and `audit`, and the Panel's inference page, call it:

```sh
openabstractions inference host add openrouter --base https://openrouter.ai/api/v1 \
  --wire openai-compatible --credential openrouter --tokens-per-day 200000
openabstractions inference key issue --for /usr/bin/python3
openabstractions inference audit
```

`host add` also writes the `complete` rule on the new host for the command line
and the Panel, and for a hosted host the runtime's own `apply` rule on its
credential. It records the host's `profiles` (`--profiles`, by default every
profile for `openai-compatible` and the local kinds, `chat` for
`anthropic-messages`) and `declared_by: operator`. A ceiling record has room for
requests, images, audio seconds and characters per day; only tokens and spend
are enforced today.

## Gateway window

Programs that only know a base URL and an API key (aider, `llm`, Codex, Claude
Code, Continue) reach the same provider through the gateway window:

```sh
openabstractions serve runtime --gateway 127.0.0.1:8765
OPENAI_BASE_URL=http://127.0.0.1:8765/v1 OPENAI_API_KEY=oalk_... <program>
ANTHROPIC_BASE_URL=http://127.0.0.1:8765 ANTHROPIC_API_KEY=oalk_... <program>
```

The window speaks OpenAI `POST /v1/chat/completions` and `GET /v1/models`, and
Anthropic `POST /v1/messages`, with server-sent events. `gateway.Listen` binds
each connection's peer through the socket-owner table
(`identity.BindLoopback`) before reading it, requires the local key issued to
that exact program before reading a body, and admits the request through the
provider as that program, so its rules decide and the audit records the window
route and the socket rung. On macOS the window refuses every connection. The
key is held in the platform store as a credentials record of kind
`openabstractions/local-key@1`; a key alone grants nothing.

The design and its rejected alternatives are in the private
`research/inference/DECISION.md`.
