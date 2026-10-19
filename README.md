# abstraction-inference

*For applications that call models. Provider authors start at
[Implement a provider or gateway](#implement-a-provider-or-gateway).*

Call text, embedding, speech, transcription, image or live-voice models without
putting a provider key or vendor protocol in the application. The application
names a model family and the behavior it needs. The runtime chooses an allowed
local engine such as Ollama or Lemonade, or a hosted server such as
OpenRouter, then applies the named credential inside the service.
Video generation and image batches use durable jobs, with retained results and
recovery after the application reconnects.

**What's new here:** Applications call local and hosted models with no API key
and no vendor protocol in the application: the service holds the credential,
checks the person's rule and records each call.

[CONTRACT.md](CONTRACT.md) states the obligations. `inference.thrift` is the
definition. The generated Go, C++, Python, Rust and JavaScript protocol code
lives under `go/`, `cpp/`, `py/`, `rs/` and `javascript/`.

The request carries messages of typed parts (text, image by storage digest,
tool call, tool result), tools, options, vendor extensions, guarantees and a
credential name. A reply streams as bounded pages: `Start` admits the call,
`Observe` reads deltas from a cursor with a long-poll wait, and `Cancel` stops
it. A request nobody observes is closed, because a stream nobody reads is
spend.

The example is in [Chat with a model](#chat-with-a-model).

## Choose an entrypoint

- An application integrating OA: a native service client, resolved through the facade.
  Check the [language and platform coverage](https://openabstractions.org/coverage.html)
  for the operations your application needs.
- Any language over HTTP: the gateway.
- OpenCode: the OA JavaScript provider.
- An AI assistant (Claude Desktop and similar MCP clients): the curated MCP
  gateway.

| Application | Entry point | Identity and policy |
| --- | --- | --- |
| OA client | Resolve the generated contract through the facade | Native caller program is checked on every call |
| OpenCode | [OA JavaScript provider](https://github.com/openabstractions/abstractions/tree/main/adopters/opencode) | OpenCode executable is the native caller |
| OpenAI/Anthropic-compatible client | Optional loopback gateway | Issued key is bound to one program; rights still decide |
| MCP client | [Curated MCP gateway](https://github.com/openabstractions/abstractions/tree/main/mcp-gateway) | MCP gateway executable is an explicit integration principal |

## Set up a model server

Lemonade, LM Studio and Ollama at their documented default local addresses
are installation declarations: the runtime reaches them with no file and no
command. `inference server add` is for a local engine at another address, or for a
hosted server. `openabstractions inference server list` shows every declared
model server, installed and operator-added alike.

Before an application's first call, one rule must grant it
`abstraction.inference/complete` on the chosen model server. A person grants
it explicitly on the command line with `rights grant`, or the Panel asks that
person at the application's first call refused for a missing rule and
records their answer as the rule from then on. The application has no path
to grant its own access.

`--program` names the built binary's absolute path. Build the application
first; `go run` starts a fresh temporary binary on every run, which never
matches a grant.

Grant one application access to a default local engine, with no model server
to declare — `ollama`, `lmstudio` or `lemonade`; `openabstractions inference
server list` shows every declared model server's exact name:

```console
openabstractions rights grant --for inference --program /absolute/path/to/app --host ollama
```

The bundle also grants `abstraction.router/inventory.read`. The same
program can list this machine's models before choosing one to chat with.

For a local engine at a non-default address, declare the model server first,
then grant the application:

```console
openabstractions inference server add ollama --base http://127.0.0.1:12345
openabstractions rights grant --for inference --program /absolute/path/to/app --host ollama
```

For a hosted server, register the credential value through standard input
for every consumer that will spend it, declare the model server using its
name, then grant the application:

```console
openabstractions credentials add openrouter --target openrouter.ai \
  --for abstraction.inference/chat@1 --for abstraction.inference/embed@1 \
  --for abstraction.inference/transcription@1 --for abstraction.inference/speech@1 \
  --for abstraction.inference/image@1 --for abstraction.inference/live@1 \
  --for abstraction.router/router@1 \
  --use-by /absolute/path/to/app --from-stdin
openabstractions inference server add openrouter \
  --base https://openrouter.ai/api/v1 --api openai-compatible \
  --credential openrouter --tokens-per-day 200000
openabstractions rights grant --for inference --program /absolute/path/to/app \
  --host openrouter --credential openrouter
```

Naming only `abstraction.inference/chat@1` and `abstraction.router/router@1`
as in earlier examples scopes the credential to chat alone: a request to
`/v1/embeddings` or a live session then reads `not_permitted` (CONTRACT.md
INF-M1, W5; inference-facade review finding 8).

The provider configuration and inference request retain `openrouter`, the
credential name. The credential manager and applier keep the value at the
service boundary. Use `openabstractions inference audit` and
`openabstractions credentials audit` to inspect attributed decisions. These
commands require an authorized operator and a running runtime.

## Where each product's address is read

`AddHost` accepts an explicit base URL for any engine. Five products declare
themselves without one, each from its own record of where it listens, and
nothing else (CONTRACT.md INF-H1):

| Product | Its own record |
| --- | --- |
| Ollama | `OLLAMA_HOST`, parsed as Ollama's own client parses it |
| LM Studio | `http-server-config.json` under `~/.lmstudio/.internal` or `~/.cache/lm-studio/.internal` |
| Docker Desktop | `settings-store.json` with `EnableInferenceTCP` true, at the documented port 12434 |
| Docker Engine (Linux) | the `docker-model` CLI plugin, at the same documented port 12434 |
| Foundry Local | the address `foundry service status` prints; it picks a port at each start and writes none to a file |

A product that recorded nothing declares no model server. Its documented
default address is an installation declaration instead: this is where
Lemonade, LM Studio, Ollama and ComfyUI at their fixed local ports live
today, with no compiled-in list inside the router itself. A malformed record
is no declaration, logged with its reason; no probe opens a connection,
scans a port or listens for multicast. llama-swap records its listen address
only in its command line, with no default configuration path, and an
operator names it explicitly with `inference server add`.

## Chat with a model

A chat completion needs inference alone. Name the model and call `Complete`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	facade "github.com/openabstractions/abstraction-facade/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	chat, err := facade.Discover().ResolveInference(ctx, facade.Requirements{})
	if err != nil {
		log.Fatal(err)
	}
	reply, err := chat.Complete(ctx, wire.Request{
		Model: os.Args[1], // the model to ask; the next example lists them
		Messages: []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{
			{Kind: wire.PartKindText, Text: "Say hello in five words."},
		}}},
	})
	if err != nil {
		log.Fatal(err)
	}
	if reply.Outcome != wire.ReplyOutcomeCompleted {
		log.Fatalf("chat %s: %s", reply.Outcome, reply.Reason)
	}
	for _, part := range reply.Message.Parts {
		fmt.Print(part.Text)
	}
}
```

`chat.Complete` starts the request, follows it to its end and returns the
reply; `chat.Stream` yields the same reply in pieces as it arrives. A missing
[Set up a model server](#set-up-a-model-server) grant refuses inside the call:
resolution succeeds, and `reply.Outcome` reads
`wire.ReplyOutcomeNotPermitted`, the check CONTRACT.md INF-A2 defines.

## List the models this machine can serve

A chat@1 request must name its model, and no default model exists yet, so an
application that does not know a name asks the router for the ones this
machine can already serve:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	facade "github.com/openabstractions/abstraction-facade/go"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	route, err := facade.Discover().ResolveRouter(ctx, facade.Requirements{})
	if err != nil {
		log.Fatal(err)
	}
	models, err := route.ModelsContext(ctx, false) // false: the cached list
	if err != nil {
		log.Fatal(err)
	}
	for _, family := range models.Models {
		for _, alias := range family.Names {
			if alias.Servable {
				fmt.Println(alias.Name)
			}
		}
	}
}
```

`route.ModelsContext` names every model this machine's model servers can
already serve; `alias.Servable` is true for one the router could route a call
to. The model layer is for acquiring weights that are not on the machine yet.

## Capabilities

The current source provides these contracts through the runtime:

| Contract | Application receives |
| --- | --- |
| `chat@1` | Text and tool-call deltas, including vision input by digest |
| `chat@2` | An explicit model or a named, caller-scoped selection policy for a chat call |
| `embed@1` | Bounded embedding vectors |
| `transcription@1` | Text and timestamped segments from stored audio |
| `speech@1` | Streamed audio and a digest-addressed final delivery |
| `image@1` | Generation or edit for an explicit model, with progress and typed image deliveries |
| `image@2` | The same image operation with an explicit model or a named selection policy |
| `live@1` | One voice turn with ordered PCM input, transcript and audio output |
| `selection-operator@2` | Read and edit policies with an optional logical provider pin; a separate `selection.manage_for` grant permits configuring another program in the bound account |

An `image@2` policy target leaves inline guarantees and credential empty; the
stored candidates provide placement and credential names. The optional provider
pin names a current logical declaration. Exact engine binary and model artifact
hashes belong to the installed adapter's configuration and provenance.

The Thrift definition generates API types and protocol bindings for Go, C++,
Python, Rust and JavaScript. The Go service owns provider communication,
credentials and output storage. The [contract page](CONTRACT.md) describes
lifecycle and authorization; generated bindings alone do not establish tested
SDK coverage.

## Go provider

- `inference.Provider` admits a request in the contract's order: shape,
  model-server selection through `router.Router` within the request's
  guarantees, features, the `abstraction.inference/complete` decision on
  `server:<name>`, authorized image reads, the credential's budget,
  capacity, then `Apply` for a hosted server. It runs the upstream request,
  keeps each request's newest deltas, cancels a request nobody observes, and
  retires ended ones after retention.
- Wires: `openai-compatible` (OpenRouter, OpenAI, Groq, LiteLLM, and the `/v1`
  of Ollama, LM Studio and Lemonade) and `anthropic-messages`, both streamed as
  server-sent events.
- `inference.Ceilings` counts tokens, spend and every other configured unit
  per credential per UTC day in an explicit state file (CONTRACT.md INF-C2).
- `inference.Config.Admit` runs after request, rights, content, budget and
  credential preflight, before accepted execution, for a provider whose own
  program has work to do on the selected model server, such as loading an
  object. A false answer keeps
  the provider's own reason word instead of turning into an upstream failure
  (CONTRACT.md INF-A5); the model host uses it in place of its own
  dispatcher.
- `inference.Provider.ActiveForModel` reports whether a request for a model
  is still active or retained for observation. A provider that loaded that
  model as an object through `Admit` can keep it loaded for as long as the
  request is open, instead of tracking calls itself.
- `service.Listen` serves the inference contracts on one endpoint and binds
  every caller by Program proof. Current macOS source uses XPC for that proof;
  the Unix-socket path remains below the required policy. The published 0.2.0
  package predates XPC support.

The same endpoint serves `transcription@1`. A request names an audio digest in
the runtime content store; the provider authorizes and verifies at most 16 MiB
for the original caller, then sends bytes to an OpenAI-compatible or Lemonade
transcription endpoint, whisper.cpp `/inference`, or Deepgram prerecorded
endpoint. Timestamp units are returned as retained segment deltas, with the
same cursor, cancellation, budget, credential and audit behavior as chat.

## Sending images to chat

An application uploads an image through the runtime's storage content writer,
then puts its digest and media type in a user message. The runtime reads the
image, verifies its digest, and sends it to the selected model. PNG, JPEG, GIF
and WebP inputs are supported by the OpenAI-compatible and Anthropic adapters.
The model must support image input.

The application needs `abstraction.storage/content.write` and
`abstraction.storage/content.read` rights for that digest, plus the model
server's `complete` right. The user runtime serves the reader and writer over
one service-owned store, with a 16 MiB limit per image and a 64 MiB total
image payload limit per request. Repeated images count toward that total.

A remote runtime needs the content in its own authorized store. Automatic
content transfer and gateway image uploads remain future work; the gateway
currently refuses image input. Native OA clients use digest references.

## Generated images, speech and live voice

Image generation and speech return typed deliveries containing a digest, media
format, size and storage location. Applications read the bytes through the
content service. Image editing names uploaded image and mask digests. The
service authorizes inputs and output publication for the original calling
program.

Live voice accepts mono PCM16 at 24 kHz through `Append`, followed by `Commit`
for one response. The client observes transcript and audio deltas. Retrying
the last acknowledged identical input frame is safe. The contract page defines
refusal and cancellation behavior when a transport outcome is uncertain.

Optional compatibility routes include `/v1/images/generations`,
`/v1/images/edits`, `/v1/audio/speech` and `/v1/realtime`.

A Realtime-speaking client opens `GET /v1/realtime?model=<name>` with its
gateway key and gets one manually committed audio turn on one `live@1`
session. The model in that URL names the model server, and the program's
permission to spend on that server is decided before the upgrade completes,
so a program without it reads an ordinary HTTP refusal rather than an error
on an open socket. `session.update` carries the voice and the PCM16 format,
`input_audio_buffer.append` the audio, `response.create` the turn, and
`response.output_audio.delta`, `response.output_audio_transcript.delta` and
`response.done` the result. The supported event subset is specified in the
contract page. Loopback tests cover that mapping, and the live-voice
measurement streams one gateway session end to end; live upstream
interoperability remains unmeasured.

## Durable video and image batches

Applications submit a generated `job.thrift` request document as job kind
`inference` through the existing job API. They keep the receipt to observe,
reconcile, cancel and read the typed result after their process exits. The
runtime uses the same caller permissions, credential manager and content
storage as other inference calls. Replicate asynchronous predictions are the
first backend. A graph-kind model server, such as ComfyUI, is Next work; see
CONTRACT.md "Next: video generation" and `research/first-run/ENGINES.md` §6
and §8.

The required `abstraction.inference/recoverable-upstream@1` guarantee records
submission intent and provider handles durably. A known handle resumes after
restart. A lost create reply remains visibly `upstream:uncertain` when the
backend cannot look up the caller's request; the runtime preserves that
uncertainty without submitting duplicate work. Each claimed job runs in its
own goroutine under its lease. A local model server runs one job at a time
and a hosted service four by default (`JobExecution.SetHostConcurrency`); a
job queued behind them shows the waiting word `host:busy` and is planned
again once its turn comes (CONTRACT.md INF-G9). Result documents contain
authorized content references. The job contract page retains its
provider-independent vocabulary.

## Packing model files

`openabstractions models pack` turns a safetensors or GGUF file, or a whole
model folder, into an OpenAbstractions model archive: a zip of STORED,
64 KiB-aligned entries that standard zip tools open, led by `oa-archive.json`
with every file's digest, 16 MiB span digests and tensor table (CONTRACT.md
INF-MP6, INF-MP9). BF16 and F16 tensors are cut into 512 KiB units, each
coded where this machine loads it faster than the plain bytes (INF-MP2);
quantised tensors, F32 and every other file are stored as they are, under
their own names. The command works on files and reaches no service.

```sh
openabstractions models pack measure model.safetensors     # this machine's rates, as --rates takes them
openabstractions models pack model.safetensors model.oa.zip
openabstractions models pack info model.oa.zip             # digests, units per encoding, bytes kept
openabstractions models pack verify model.oa.zip --digest sha256:<hex>
openabstractions models pack restore model.oa.zip model.safetensors   # byte for byte
openabstractions models pack ./my-model my-model.oa.zip    # a folder: one archive
openabstractions models pack restore my-model.oa.zip ./my-model-again
```

Without `--rates`, `pack` measures for a few seconds first. Each unit takes
`stored`, `exp-huffman-v1` (about 0.67 of BF16 kept) or, for BF16,
`exp-fixed3-v1` (about 0.70 kept, several times faster to decode per core),
whichever loads soonest on this machine. A file with encoded units is the
entry `<path>.oa-units`, which `restore` turns back into the file; a file
whose units all stay stored is its own entry, which any zip tool extracts as
the file. An archive moved to another machine stays valid there and may be
packed again with that machine's rates. Pickles (`.pth`, `.pt`, `.ckpt`,
`.bin`) are refused: loading one runs code.

## Runtime configuration

The user runtime (`openabstractions serve runtime`) composes the provider with
its rights policy and credentials manager. Model servers come from
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
Lemonade, LM Studio and Ollama at their default addresses. A hosted server's
listing is read under the runtime's own program: its credential needs an
`apply` rule for that program and consumer `abstraction.router/router@1`; each
application needs its own `apply` rule and an `abstraction.inference/complete`
rule on `server:<name>`. `host add` writes the runtime program's own `apply`
rule when it declares a model server whose credential is already registered;
the credential's own `Store` writes it when the credential is registered
after the model server is already declared (CONTRACT.md INF-P2). Either order
reaches the same rule; a survey reports a missing one by naming the runtime
program in the model server's `down` reason, `credential:<outcome> for
<program>`.

`abstraction.inference/operator@1`, served on the same endpoint, edits that
file and reads the audit. `openabstractions inference server add|list|remove`,
`key issue|revoke|list` and `audit`, and the Panel's inference page, call it:

```sh
openabstractions inference server add openrouter --base https://openrouter.ai/api/v1 \
  --api openai-compatible --credential openrouter --tokens-per-day 200000
openabstractions inference key issue --for /usr/bin/python3
openabstractions inference audit
```

`host add` also writes the `complete` rule on the new model server for the
command line and the Panel, and for a hosted server the runtime's own `apply`
rule on its credential. It records the model server's modalities
(`--modalities`, by default the wire's own table: every modality for
`openai-compatible` and the local kinds, `live` for `openai-realtime`,
`transcription` for `deepgram-prerecorded`, `speech` for
`elevenlabs-stream`, `image` for `stability-v2beta`, `fal-queue` and
`replicate-predictions`, `chat` for `anthropic-messages`) and
`declared_by: operator`. A budget record has room for requests, images,
audio seconds and characters per day, and this version enforces every unit
it configures (CONTRACT.md INF-C2).

## Gateway

Programs that only know a base URL and an API key (aider, `llm`, Codex, Claude
Code, Continue) reach the same provider through the gateway. On an installed
runtime, turn the gateway on once; the setting stays on across restarts:

```sh
openabstractions inference gateway on --address 127.0.0.1:8765
OPENAI_BASE_URL=http://127.0.0.1:8765/v1 OPENAI_API_KEY=oalk_... <program>
ANTHROPIC_BASE_URL=http://127.0.0.1:8765 ANTHROPIC_API_KEY=oalk_... <program>
```

`openabstractions serve runtime --gateway 127.0.0.1:8765` is the foreground
development command: it opens the gateway on that one manually started
runtime for that run, and leaves the installed setting unchanged.

The gateway speaks OpenAI `POST /v1/chat/completions` and `GET /v1/models`,
and Anthropic `POST /v1/messages`, with server-sent events. `gateway.Listen`
binds each connection's peer through the socket-owner table
(`identity.BindLoopback`) before reading it, requires the gateway key issued
to that exact program before reading a body, and admits the request through
the provider as that program: its rules decide, and the audit records the
gateway route and the socket rung. On macOS the gateway still refuses every
connection: XPC service proof does not establish loopback caller proof. The
key is held in the platform store as a credentials record of
kind `openabstractions/local-key@1`; a key alone grants nothing, and which
consumer it admits for follows the route, not the key itself (CONTRACT.md
INF-W5).

A Realtime-speaking client reaches live voice the same way:

```sh
openabstractions inference key issue --for <program> --credential <name>
# ws://127.0.0.1:8765/v1/realtime?model=<name>, Authorization: Bearer oalk_...
```

## Implement a provider or gateway

The [Go core](go/) implements inference policy and service behavior. The runtime
supplies vendor connections and compatibility gateways from the separate
[adapter module](adapters/go/). An application resolves the native inference
client through the facade; the runtime selects its provider.

A realtime adapter implements `LiveConnection` and supplies `Config.LiveDialer`.
The core admits the operation and passes a credential-protected HTTP client to
that dialer. Audio formats, event kinds, terminal outcomes and adapter errors
use typed options. The core owns cancellation and session cleanup.

The gateway's current import is
`github.com/openabstractions/abstraction-inference/adapters/go/gateway`.
See the [adapter README](adapters/go/README.md) for migration and dependency
status. The [contract](CONTRACT.md) defines the behavior each provider must keep.

<!-- BEGIN GENERATED PERMISSION DEFINITIONS -->

## Permission definitions

Generated from the existing compiled catalogue. [RIGHTS.json](RIGHTS.json) contains the complete action/profile records, disclosure, lifecycle, first-use behavior, selector support and finite bundles.

This reference records compiled receiving support; RIGHTS.json names the compilation context. A connected runtime advertises current availability and selector support. These definitions supply no grant.

| Action | Resource profile | Owning operations | First-use targets | Compiled receiver |
|---|---|---|---|---|
| `abstraction.inference/audit.read` | `abstraction.rights/legacy-exact-string@1` | `abstraction.inference/operator@1#Audit` | `none` | served in compiled context |
| `abstraction.inference/complete` | `abstraction.rights/legacy-exact-string@1` | `abstraction.inference/chat@1#Start`, `abstraction.inference/chat@2#Start`, `abstraction.inference/preload@1#Preload`, `abstraction.inference/embed@1#Embed`, `abstraction.inference/transcription@1#Start`, `abstraction.inference/speech@1#Start`, `abstraction.inference/live@1#Start`, `abstraction.inference/image@1#Start`, `abstraction.inference/image@2#Start`, `abstraction.inference/video@1#Start` | Legacy `exact` | served in compiled context |
| `abstraction.inference/host.manage` | `abstraction.rights/legacy-exact-string@1` | `abstraction.inference/operator@1#Hosts`, `abstraction.inference/operator@1#AddHost`, `abstraction.inference/operator@1#RemoveHost`, `abstraction.inference/operator@1#Gateway`, `abstraction.inference/operator@1#SetGateway` | `none` | served in compiled context |
| `abstraction.inference/key.issue` | `abstraction.rights/legacy-exact-string@1` | `abstraction.inference/operator@1#Keys`, `abstraction.inference/operator@1#IssueKey`, `abstraction.inference/operator@1#RevokeKey` | `none` | served in compiled context |
| `abstraction.inference/selection.manage` | `abstraction.rights/legacy-exact-string@1` | `abstraction.inference/selection-operator@2#Read`, `abstraction.inference/selection-operator@2#Put`, `abstraction.inference/selection-operator@2#DeletePolicy` | `none` | served in compiled context |
| `abstraction.inference/selection.manage_for` | `abstraction.rights/legacy-exact-string@1` | `abstraction.inference/selection-operator@2#Read`, `abstraction.inference/selection-operator@2#Put`, `abstraction.inference/selection-operator@2#DeletePolicy` | `none` | served in compiled context |

<!-- END GENERATED PERMISSION DEFINITIONS -->
