# Nautilus-TS

A TypeScript framework for building applications inside [AWS Nitro Enclaves](https://aws.amazon.com/ec2/nitro/nitro-enclaves/) with first-class [Sui](https://sui.io) support.

Write your enclave business logic in TypeScript using Mysten's SDKs (`@mysten/sui`, `@mysten/seal`, etc.), and the framework handles all the enclave plumbing: VSOCK networking, NSM attestation, traffic forwarding, and key management.

## Architecture

```
┌──────────────────── Nitro Enclave ────────────────────┐
│                                                        │
│  ┌──────────────┐     ┌──────────────────────────┐    │
│  │ traffic-     │     │ nautilus-server           │    │
│  │ forwarder    │     │ (Bun compiled binary)     │    │
│  │ (Rust)       │     │                           │    │
│  │              │     │  Your TS business logic   │    │
│  │ VSOCK:3000 ──┼─TCP─┤► Bun.serve() on :3000    │    │
│  │              │     │  @mysten/sui, @mysten/seal│    │
│  │ TCP:443 ◄────┼─────┤► fetch("https://...")     │    │
│  │  ↕ VSOCK     │     │                           │    │
│  └──────────────┘     │  NSM attestation via FFI  │    │
│                       │  to libnsm_ffi.so (Rust)  │    │
│                       └──────────────────────────┘    │
│                                                        │
└───────────────────── VSOCK boundary ──────────────────┘
         ↕                              ↕
┌──────── EC2 Host ─────────────────────────────────────┐
│  socat TCP:8080 ↔ VSOCK:3000     (inbound HTTP)      │
│  vsock-proxy VSOCK:8101 ↔ seal.mirai.cloud:443       │
│  vsock-proxy VSOCK:8103 ↔ walrus.space:443           │
│  vsock-proxy VSOCK:8104 ↔ fullnode.sui.io:443        │
└───────────────────────────────────────────────────────┘
```

### Components

**Bun compiled binary** (`nautilus-server`): Your TypeScript application compiled into a single ~55MB standalone binary via `bun build --compile`. Contains the Bun runtime, all npm dependencies, and your business logic. Runs as PID 1 target inside the enclave.

**Rust traffic forwarder** (`traffic-forwarder`): A small static binary that handles all TCP↔VSOCK bridging using tokio's async runtime. Spawned as a child process by the Bun binary at boot. Manages:

- **Inbound bridge**: VSOCK:3000 → TCP:127.0.0.1:3000 (HTTP requests from the host reach your Bun server)
- **Outbound forwarders**: TCP:127.0.0.x:443 → VSOCK:parent:port (your `fetch()` calls reach external services)
- **/etc/hosts**: Maps endpoint hostnames to loopback addresses so `fetch("https://fullnode.testnet.sui.io")` resolves correctly

**Rust NSM FFI** (`libnsm_ffi.so`): Minimal shared library (~200KB) that exposes NSM attestation and hardware RNG to TypeScript via `bun:ffi`. Only two functions: `nsm_get_attestation()` and `nsm_get_random()`. Everything else (Ed25519 signing, hashing, Sui transactions) runs in TypeScript.

## Why TypeScript Instead of Pure Rust?

The [official Mysten Nautilus implementation](https://github.com/MystenLabs/nautilus) is written entirely in Rust. We chose a TypeScript-first approach for different reasons:

**SDK access**: Mysten's TypeScript SDKs (`@mysten/sui`, `@mysten/seal`, `@mysten/bcs`) are designed for building user-facing applications quickly. The Rust SDKs are actively maintained and complete, but implemented as lower-level building blocks — Seal decryption flows, PTB construction helpers, and BCS encoding are more ergonomic in TypeScript. With Bun, you import the same libraries you use in your frontend/backend.

**Selective Rust**: Rust is used only where it's genuinely better:

- **NSM attestation** requires ioctl on `/dev/nsm` — a Rust cdylib is the minimal, correct way to expose this
- **Traffic forwarding** requires async socket I/O across two address families (TCP + VSOCK) — tokio-vsock provides battle-tested async VSOCK support that doesn't exist in the JS ecosystem

## Why a Custom Traffic Forwarder?

AWS's Nitro Enclave documentation and Mysten's official implementation both use [traffic_forwarder.py](https://github.com/MystenLabs/nautilus/blob/main/src/nautilus-server/traffic_forwarder.py), a Python script that:

- Spawns threads for each connection direction
- Uses blocking `recv(1024)` in a loop
- Requires the Python runtime inside the enclave (~40MB)

We replaced it with a Rust binary for several reasons:

**No Python in the enclave**: Adding Python to a minimal enclave image adds ~40MB, increases the attack surface, and makes reproducible builds harder. Our traffic forwarder is a single static musl binary (~2MB).

**Proper async I/O**: Python threads with blocking `recv()` work but are not optimal. Our Rust implementation uses `tokio::io::copy` with `tokio-vsock` — epoll-based async I/O that handles backpressure correctly and uses zero CPU when idle.

**Single process model**: The Python forwarder runs as a separate script with its own lifecycle. Ours is spawned by the Bun process and monitored — if it crashes, the enclave shuts down cleanly instead of silently losing network connectivity.

**Template quality**: This is meant to be a starting template. Including a Python script alongside TypeScript and Rust sends the wrong signal about the architecture. Each language in the enclave should earn its place.

## VSOCK and FFI

Nitro Enclaves have no network interface. The only communication channel is [VSOCK](https://man7.org/linux/man-pages/man7/vsock.7.html) — a socket address family (`AF_VSOCK = 40`) for hypervisor-guest communication.

**Boot config** (one-shot): At startup, the Bun binary uses `bun:ffi` to call libc's `socket()`, `bind()`, `listen()`, `accept()`, and `read()` with `AF_VSOCK` to receive a JSON configuration blob from the host via VSOCK port 7777. This is a simple blocking operation that happens once.

**Traffic forwarding** (persistent): All ongoing VSOCK I/O is handled by the Rust traffic forwarder using `tokio-vsock`. The Bun process only deals with standard TCP sockets — `Bun.serve()` listens on `127.0.0.1:3000`, and `fetch()` calls resolve to loopback addresses that the traffic forwarder bridges to VSOCK.

**NSM attestation**: The `/dev/nsm` device requires an ioctl interface. A minimal Rust cdylib (`libnsm_ffi.so`) wraps the [aws-nitro-enclaves-nsm-api](https://github.com/aws/aws-nitro-enclaves-nsm-api) crate and exposes C functions callable from `bun:ffi`. TypeScript calls `nsm_get_attestation(public_key)` and gets back a CBOR-encoded attestation document.

**libc compatibility**: The enclave uses musl libc (via StageX), while development machines use glibc. The FFI layer handles this by trying `libc.so.6` first (glibc) and falling back to `libc.so` (musl).

## Quick Start

```bash
# Install dependencies
bun install

# Local development (no enclave)
bun run dev

# Build EIF (requires Docker)
make

# Deploy to EC2 with Nitro Enclaves
scp out/nitro.eif ec2-user@<host>:~/nautilus-ts/out/
ssh ec2-user@<host> "cd nautilus-ts && scripts/deploy.sh"
```

## Project Structure

```
src/
  server.ts          # Your application entry point
  nautilus.ts        # Framework: boot, routing, HTTP server
  core/
    vsock.ts         # Minimal VSOCK FFI (config reception only)
    config.ts        # Boot config via VSOCK:7777
    network.ts       # Loopback interface setup
    crypto.ts        # Ed25519 signing, hashing (@noble libraries)
  nsm/
    index.ts         # NSM attestation via Rust FFI
enclave/
  nsm-ffi/           # Rust cdylib for /dev/nsm attestation
  traffic-forwarder/ # Rust binary for TCP↔VSOCK bridging
scripts/
  deploy.sh          # EC2 deployment script
  systemd/           # systemd units for host services
Containerfile        # Multi-stage EIF build (StageX + Bun + Rust)
```

## Configuration

The enclave receives configuration at boot via VSOCK port 7777:

```json
{
  "endpoints": [
    { "host": "fullnode.testnet.sui.io", "vsock_port": 8104 },
    { "host": "open.key-server.testnet.seal.mirai.cloud", "vsock_port": 8101 }
  ],
  "app": {
    "sui_network": "testnet",
    "custom_key": "custom_value"
  }
}
```

- `endpoints`: External services the enclave needs to reach. Each gets a VSOCK-forwarded loopback address.
- `app`: Arbitrary application config, available via `ctx.config.app` in your route handlers.
- `secrets`: Key-value pairs injected into `process.env`.

## Writing Routes

```ts
import { Nautilus } from "./nautilus.ts";

const app = new Nautilus();

app.post("/process", async (req, ctx) => {
  const body = await req.json();

  // Use Mysten SDKs normally
  const tx = new Transaction();
  // ...

  // Sign with ephemeral enclave keypair
  const signature = ctx.sign(someData);

  return Response.json({
    result: "processed",
    signature: ctx.toHex(signature),
    public_key: ctx.publicKey,
  });
});

app.start();
```

The `ctx` object provides:

- `ctx.sign(bytes)` — Ed25519 signature with ephemeral keypair
- `ctx.publicKey` / `ctx.address` — Hex-encoded public key and Sui address
- `ctx.config` — Boot configuration
- `ctx.attest()` — NSM attestation document (enclave only)
- `ctx.toHex()` / `ctx.fromHex()` / `ctx.blake2b256()` / `ctx.sha256()` — Crypto utilities

## FAQ

### How does the enclave access the NSM for attestation?

The NSM (Nitro Secure Module) is accessed via ioctl on `/dev/nsm`. Since JavaScript can't do ioctl directly, we compile a minimal Rust shared library (`libnsm_ffi.so`) that wraps the `aws-nitro-enclaves-nsm-api` crate. TypeScript calls it through `bun:ffi`. You could also do this from bash, Python, or any language that can open a device file and issue ioctls — Rust is just the cleanest option for a compiled cdylib.

### Does the TypeScript code know it's inside an enclave?

Mostly no, by design. Your `fetch("https://fullnode.testnet.sui.io/...")` calls resolve normally — `/etc/hosts` maps hostnames to loopback addresses, and the Rust traffic forwarder bridges those to VSOCK transparently. The only enclave-aware code is the boot sequence and the `ctx.attest()` call. Your business logic reads like a normal TypeScript server.

### Should I assume the EC2 host is malicious?

Yes. The host controls the network, can inspect or modify unencrypted traffic, and sends the boot configuration. This is exactly why enclaves exist — the attestation document cryptographically proves what code is running and binds your ephemeral public key to that measurement. Design your protocol so that all trust flows from the attestation, not from the host.

### How does the enclave handle TLS certificates?

Trusted CA certificates from StageX's `core-ca-certificates` package are baked into the EIF image at build time. The environment variables `SSL_CERT_FILE` and `SSL_CERT_DIR` point to these embedded certs. Since they're part of the measured image (reflected in PCR values), the host cannot replace or tamper with them. All outbound HTTPS connections verify against these embedded roots.

### Can the host tamper with inbound HTTP requests to the enclave?

Yes — inbound traffic from the host to the enclave's HTTP server is unencrypted (it travels over VSOCK, which is a local hypervisor channel, not a network). This is by design and matches Mysten's official implementation. The security model relies on the enclave **signing its responses** with its attestation-bound keypair. The on-chain Move contract verifies that signature, so it doesn't matter if the host tampers with requests — only correctly signed enclave outputs are accepted on-chain.

### How is the ephemeral keypair generated? Is it secure?

The keypair is generated at boot by mixing two independent entropy sources: `crypto.getRandomValues()` (kernel PRNG) and the NSM hardware RNG (`/dev/nsm`). These are XORed together, so both sources must be compromised to predict the key. In dev mode (no NSM), only the kernel PRNG is used. The keypair is ephemeral — it lives only in enclave memory and is destroyed when the enclave terminates.

### Are builds reproducible? Will PCR values be the same?

Yes, given identical source code and lockfiles. All Docker base images are pinned by SHA256 digest, Cargo.lock and bun.lock are committed, `cargo build --locked` and `bun install --frozen-lockfile` are enforced, file timestamps are zeroed, and cpio archives are built with deterministic ordering. Run `bun scripts/check-reproducibility.ts` to verify that nothing has drifted. The same source should produce the same PCR0/PCR1/PCR2 measurements on any machine.

### What are the PCR values and how do I verify them?

PCR0 measures the enclave image (your code + all dependencies). PCR1 measures the kernel. PCR2 measures the application. After building (`make`), the measurements are written to `out/nitro.pcrs`. Anyone can clone the repo, run `make`, and compare their PCR values against yours. If they match, the enclave is running the published source code.

### Can I add more external endpoints?

Yes. Add entries to the `endpoints` array in your boot config and set up corresponding `vsock-proxy` instances on the host. Each endpoint gets a unique loopback IP (`127.0.0.64`, `127.0.0.65`, etc.) and an entry in `/etc/hosts`. Your code just calls `fetch("https://your-service.com/...")` as usual.

### Why Bun instead of Node.js?

Bun compiles TypeScript into a single standalone binary via `bun build --compile`. This binary includes the runtime, all npm dependencies, and your code — no `node_modules` directory, no package manager, no interpreter needed inside the enclave. It also has native FFI support (`bun:ffi`) which we use for both VSOCK and NSM access.

### Why not just use the official Mysten Nautilus (Rust)?

You can and should if Rust is your preference. This template exists for teams that want to write their enclave business logic in TypeScript using Mysten's TypeScript SDKs. The trade-off is a slightly larger image (Bun runtime + Rust components vs pure Rust) in exchange for faster iteration and access to the TypeScript ecosystem.

### What happens if the traffic forwarder crashes?

The Bun process monitors the traffic forwarder as a child process. If it exits, Bun logs the error and calls `process.exit(1)`, which terminates the enclave. This is intentional — silently losing network connectivity is worse than a clean restart.

### Can I use this without Sui?

Yes. The framework provides generic enclave utilities (attestation, signing, hashing, encrypted config). The Sui-specific parts (`@mysten/sui`, `suiAddress()`) are optional — remove them from `package.json` and `crypto.ts` if you don't need them.

### How do I pass secrets to the enclave?

Include a `secrets` object in the boot config sent via VSOCK:7777. These are injected into `process.env` at startup. Note that the boot config is sent by the host, so secrets are only as secure as your trust model. For highly sensitive material, consider using Seal encryption with the enclave's attestation-bound identity.

### Is the Rust traffic forwarder necessary? Can I use Python's traffic_forwarder.py?

You can, but we don't recommend it. The Python forwarder adds ~40MB to the image, uses blocking threads, and requires the Python runtime. Our Rust forwarder is a 2MB static binary with async I/O. It also means one fewer language runtime in your attack surface.

### How does this compare to Mysten's Nautilus for reproducibility?

Both implementations use the same StageX-based reproducible build pipeline: SHA256-pinned images, deterministic cpio (zeroed timestamps, sorted entries, `--reproducible` flag), `eif_build` for the final EIF, and `--provenance=false` in Docker. This template additionally includes an automated reproducibility verification script and CI workflow.

### Why generate a new keypair on every boot instead of loading one?

This is fundamental to the enclave security model. The attestation document binds the public key to the enclave's PCR measurements, proving "this key was generated inside code with these exact measurements." If you loaded a key from the host (e.g., `.env` or config), the host would know the private key, and the attestation would prove nothing — the host could sign anything. Ephemeral keys generated inside the enclave's isolated memory are the only kind that the attestation can meaningfully vouch for. The trade-off is that the enclave's Sui address changes on every reboot and must be re-approved on-chain.

### Can I run this locally for development?

Yes. `bun run dev` starts the server in dev mode — no enclave, no VSOCK, no NSM. It reads config from a local file (or uses defaults), generates a keypair from the kernel PRNG, and listens on `localhost:3000`. The `ctx.attest()` call returns `null` in dev mode.

## Disclaimer

This framework is provided as-is and has not been audited. We are not responsible for any issues arising from its use. Use at your own risk.

Contributions and comments are welcome — please open an issue or pull request.
