---
description: Nautilus TypeScript enclave framework - uses Bun, not Node.js.
globs: "*.ts, *.tsx, *.js, package.json, Containerfile, Makefile"
alwaysApply: true
---

Use Bun instead of Node.js. No `bun:ffi` — native boundaries use the `argonaut` companion binary.

## Architecture

- `src/core/` — Platform layer (VSOCK, networking, crypto, config)
- `src/nsm/` — NSM attestation via persistent argonaut proxy process
- `src/nautilus.ts` — boot() function, Hono app setup, NautilusContext
- `src/server.ts` — Application entry point (user code)
- `argonaut/` — Go binary for TCP↔VSOCK bridging, NSM attestation, and config delivery

## Key Decisions

- Ed25519 signing in TypeScript (@noble/ed25519), NOT native code
- NSM attestation via Go ioctl on /dev/nsm (argonaut binary)
- VSOCK via Go argonaut binary (no socat/Python/FFI inside enclave)
- Config via VSOCK:7777 at boot (dynamic, not baked into image)
- `bun build --compile` for single-binary output
- `nit.target=/nautilus-server` — Bun binary runs directly as init target
- Hono for HTTP routing, served via Bun

## Build

```sh
make              # builds EIF via Docker
bun run dev       # local dev mode with hot reload
```
