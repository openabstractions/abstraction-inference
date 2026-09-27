# abstraction-inference

Call text, embedding, speech, transcription, image or live-voice models without
putting a provider key or vendor protocol in the application. The application
names a model family and the behavior it needs. The runtime chooses an allowed
local engine such as Ollama or Lemonade, or a hosted server such as
OpenRouter, then applies the named credential inside the service.
Video generation and image batches use durable jobs, with retained results and
recovery after the application reconnects.

[CONTRACT.md](CONTRACT.md) states the obligations. `inference.thrift` is the
definition. The generated Go, C++, Python, Rust and JavaScript protocol code
lives under `go/`, `cpp/`, `py/`, `rs/` and `javascript/`.

The request carries messages of typed parts (text, image by storage digest,
tool call, tool result), tools, options, vendor extensions, guarantees and a
credential name. A reply streams as bounded pages: `Start` admits the call,
`Observe` reads deltas from a cursor with a long-poll wait, and `Cancel` stops
it. A request nobody observes is closed, because a stream nobody reads is
spend.

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

## List this machine's models, then chat with one

A chat completion needs inference alone. The router is optional, for
choosing a model name from what this machine can already serve; the model
layer is for acquiring weights that are not on the machine yet.

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	facade "github.com/openabstractions/abstraction-facade/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	route, err := facade.Discover().ResolveRouter(ctx, facade.Requirements{})
	if err != nil {
		log.Fatal(err)
	}
	models, err := route.ModelsContext(ctx, false) // false: the cached list; true asks every model server again
	if err != nil {
		log.Fatal(err)
	}

	var name string
find:
	for _, family := range models.Models {
		for _, alias := range family.Names {
			if alias.Servable {
				name = alias.Name
				break find
			}
		}
	}
	if name == "" {
		log.Fatal("no servable model on this machine")
	}

	chat, err := facade.Discover().ResolveInference(ctx, facade.Requirements{})
	if err != nil {
		log.Fatal(err)
	}
	request := wire.Request{
		Model: name,
		Messages: []wire.Message{
			{Role: wire.RoleUser, Parts: []wire.Part{
				{Kind: wire.PartKindText, Text: "Say hello in five words."},
			}},
		},
	}
	for delta, err := range chat.Stream(ctx, request) {
		if err != nil {
			log.Fatal(err)
		}
		if delta.Kind != wire.DeltaKindEnd {
			continue
		}
		reply := delta.End
		if reply.Outcome == wire.ReplyOutcomeNotPermitted {
			log.Fatalf("no rights rule lets this program use this model server: %s", reply.Reason)
		}
		if reply.Outcome != wire.ReplyOutcomeCompleted {
			log.Fatalf("chat %s: %s", reply.Outcome, reply.Reason)
		}
		for _, part := range reply.Message.Parts {
			fmt.Print(part.Text)
		}
	}
}
```

`route.ModelsContext` names every model this machine's model servers can
already serve; `alias.Servable` is true for one this router could route a
call to. `chat.Stream` then takes that name the same way
[the facade README's inference example](https://github.com/openabstractions/abstraction-facade/blob/main/README.md#go)
does. A missing [Set up a model server](#set-up-a-model-server) grant refuses
inside the call itself, not at resolution: `reply.Outcome` above reads
`wire.ReplyOutcomeNotPermitted`, the check CONTRACT.md INF-A2 defines.

## Capabilities

The current source provides these contracts through the runtime:

| Contract | Application receives |
| --- | --- |
| `chat@1` | Text and tool-call deltas, including vision input by digest |
| `embed@1` | Bounded embedding vectors |
| `transcription@1` | Text and timestamped segments from stored audio |
| `speech@1` | Streamed audio and a digest-addressed final delivery |
| `image@1` | Generation or edit progress and typed image deliveries |
| `live@1` | One voice turn with ordered PCM input, transcript and audio output |

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
- `inference.Config.Admit` runs after the model server is picked and before
  the rights decision, for a provider whose own program has work to do on
  that model server first, such as loading an object. A false answer keeps
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
uncertainty without submitting duplicate work. Result documents contain
authorized content references. The job contract page retains its
provider-independent vocabulary.

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
