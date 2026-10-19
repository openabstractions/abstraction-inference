# Independent native image provider example

*For provider authors writing Go.*

Build this example from the inference module with `go build ./examples/image-provider`. Run it with an OA native endpoint, the absolute OA runtime executable path, and a provider-owned store directory:

```text
image-provider -endpoint <native-endpoint> -runtime <absolute-runtime-executable> -store <absolute-provider-store>
```

Declare the process as a native provider with `abstraction.inference/image@1`, `abstraction.storage/content-reader@1`, `abstraction.storage/content-writer@1`, resource `profile:image`, and model `sample/solid-pixel`. The runtime mediates application requests to this endpoint. The provider returns a one-pixel PNG so the example has no external engine dependency. Replace `solidPixel.Unsupported` and `solidPixel.Run` with an engine adapter; keep `providerhost.NewImage` for authenticated IPC, on-demand admission, bounded content transfer, and operation lifetime. For an owned process engine, supply `ImageConfig.Admit` to start/load after request preflight and `ImageConfig.HTTP` only when using a guarded local HTTP engine client.

The provider's output digest belongs to its own store. The OA runtime's native image mediator reads and verifies it through the bound content reader, then publishes a new delivery into the caller-authorized store. Applications connect to the runtime's mediated endpoint, not this leaf endpoint.
