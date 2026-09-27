# Inference provider adapters

This module contains the vendor Realtime WebSocket connector and the
OpenAI/Anthropic-compatible loopback gateway. The inference core module owns
admission, rights, budgets, credential application and redirect protection. The
runtime composes `realtime.Dial` through `inference.Config.LiveDialer` and opens
`gateway.Window` after service setup. Native OA clients and remote providers
continue to use their existing service transport.

The gateway import moved from
`github.com/openabstractions/abstraction-inference/go/gateway` to
`github.com/openabstractions/abstraction-inference/adapters/go/gateway`. The
former package no longer exists; runtime consumers must update that import.

The WebSocket transport uses the [OpenAbstractions Gorilla fork](https://github.com/openabstractions/websocket)
at commit `ce87d3641bd1e04e7acfd4a848d193951d5da73f` (the exact Go module
version is in `go.mod`). Its [OA-FORK.md](https://github.com/openabstractions/websocket/blob/ce87d3641bd1e04e7acfd4a848d193951d5da73f/OA-FORK.md)
records the Gorilla v1.5.3 upstream commit, the narrow post-upgrade constructor,
license, and maintenance provenance. The adapter [NOTICE](NOTICE) records its
redistribution attribution. OpenAbstractions maintainers own upstream security
update review and must requalify this pinned fork before advancing it.
