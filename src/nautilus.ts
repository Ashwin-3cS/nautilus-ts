/**
 * Nautilus — TypeScript enclave framework.
 *
 * ```ts
 * import { boot } from "./nautilus.ts";
 *
 * const { app, ctx } = await boot({ port: 3000 });
 *
 * app.post("/my_endpoint", async (c) => {
 *   const body = await c.req.json();
 *   const sig = ctx.sign(ctx.blake2b256(data));
 *   return c.json({ signature: ctx.toHex(sig) });
 * });
 *
 * export default { port: 3000, hostname: "127.0.0.1", fetch: app.fetch };
 * ```
 */

import { Hono } from "hono";
import {
  type BootConfig,
  receiveBootConfig,
  devBootConfig,
  setupLoopback,
  generateKeypair,
  sign,
  suiAddress,
  toHex,
  fromHex,
  blake2b256,
  sha256Hash,
} from "./core/index.ts";
import { isEnclave, getAttestation, getHardwareRandom, stopNsmProxy } from "./nsm/index.ts";

export interface NautilusContext {
  /** Hex-encoded public key. */
  publicKey: string;
  /** Sui address derived from the public key. */
  address: string;
  /** Boot config received from host. */
  config: BootConfig;
  /** Whether we're running inside a Nitro Enclave. */
  inEnclave: boolean;
  /** Sign bytes with the ephemeral keypair. */
  sign(message: Uint8Array): Uint8Array;
  /** Get NSM attestation document (null if not in enclave). */
  attest(): Promise<Uint8Array | null>;
  /** Hex encode. */
  toHex: typeof toHex;
  /** Hex decode. */
  fromHex: typeof fromHex;
  /** Blake2b-256 hash. */
  blake2b256: typeof blake2b256;
  /** SHA-256 hash. */
  sha256: typeof sha256Hash;
  /** Clean up resources (NSM proxy). Call when shutting down. */
  shutdown(): void;
}

// ── In-memory log ring buffer ────────────────────────────────────────

class LogBuffer {
  private lines: string[] = [];
  private capacity: number;

  constructor(capacity = 1000) {
    this.capacity = capacity;
  }

  push(line: string) {
    if (this.lines.length >= this.capacity) {
      this.lines.shift();
    }
    this.lines.push(line);
  }

  recent(n: number): string[] {
    const capped = Math.min(n, this.lines.length);
    return this.lines.slice(-capped);
  }
}

export interface BootOptions {
  /** Port the HTTP server will listen on (default: 3000). */
  port?: number;
  /** Path to a local config file for dev mode. */
  devConfigPath?: string;
  /** @internal Override enclave detection for testing. */
  _testAsEnclave?: boolean;
}

export interface BootResult {
  /** Hono app with GET /attestation (NSM hardware attestation). App owns all other routes. */
  app: Hono;
  /** Enclave context: signing, attestation, config, crypto utilities. */
  ctx: NautilusContext;
}

/** Spawn the argonaut companion as a child process. */
function startArgonaut(config: BootConfig, httpPort: number): void {
  const proxyConfig = JSON.stringify({
    httpVsockPort: httpPort,
    httpTcpPort: httpPort,
    endpoints: config.endpoints,
  });

  const proc = Bun.spawn(["/argonaut", "enclave"], {
    stdin: new Blob([proxyConfig]),
    stdout: "inherit",
    stderr: "inherit",
  });

  proc.exited.then((code) => {
    console.error(`[nautilus] argonaut companion exited with code ${code}`);
    process.exit(1);
  });

  console.log(`[nautilus] argonaut companion started (pid ${proc.pid})`);
}

/**
 * Boot the enclave and return a Hono app with context.
 *
 * In enclave mode:
 *   1. Set up loopback networking
 *   2. Receive config from host via VSOCK:7777
 *   3. Spawn argonaut (handles /etc/hosts, TCP↔VSOCK bridges)
 *
 * In dev mode:
 *   1. Read config from file or use defaults
 *
 * Returns a Hono app (with GET /attestation for NSM hardware attestation)
 * plus the NautilusContext for signing and attestation.
 */
export async function boot(options: BootOptions = {}): Promise<BootResult> {
  const port = options.port ?? 3000;
  const inEnclave = options._testAsEnclave || isEnclave();
  let config: BootConfig;

  // Set up log capture — intercept console.log/error to feed the ring buffer
  const logBuffer = new LogBuffer(1000);
  const origLog = console.log.bind(console);
  const origError = console.error.bind(console);

  const captureLog = (...args: unknown[]) => {
    const line = `${new Date().toISOString()} INFO  ${args.map(String).join(" ")}`;
    logBuffer.push(line);
    origLog(...args);
  };
  const captureError = (...args: unknown[]) => {
    const line = `${new Date().toISOString()} ERROR ${args.map(String).join(" ")}`;
    logBuffer.push(line);
    origError(...args);
  };
  console.log = captureLog;
  console.error = captureError;

  if (inEnclave && !options._testAsEnclave) {
    console.log("[nautilus] booting in enclave mode");
    setupLoopback();
    config = await receiveBootConfig();

    // Spawn argonaut — handles /etc/hosts, inbound + outbound bridges
    startArgonaut(config, port);
  } else {
    console.log("[nautilus] booting in dev mode");
    config = await devBootConfig(options.devConfigPath);
  }

  // Generate ephemeral keypair (mix NSM hardware entropy when available)
  const nsmEntropy = inEnclave ? await getHardwareRandom() : null;
  const keypair = generateKeypair(nsmEntropy);
  const publicKey = toHex(keypair.publicKey);
  const address = suiAddress(keypair.publicKey);

  console.log(`[nautilus] public key: ${publicKey}`);
  console.log(`[nautilus] address:    ${address}`);

  const ctx: NautilusContext = {
    publicKey,
    address,
    config,
    inEnclave,
    sign: (msg) => sign(keypair, msg),
    attest: () => getAttestation(keypair.publicKey),
    toHex,
    fromHex,
    blake2b256,
    sha256: sha256Hash,
    shutdown: () => stopNsmProxy(),
  };

  // Create Hono app — only NSM attestation is a framework route.
  // The app owns all other routes (health_check, business logic, etc.).
  const app = new Hono();

  app.get("/logs", (c) => {
    const n = Math.min(Number(c.req.query("lines") ?? 100), 1000);
    const lines = logBuffer.recent(n);
    return c.json({ lines, count: lines.length });
  });

  app.get("/attestation", async (c) => {
    const doc = await ctx.attest();
    if (!doc) {
      return c.json({ error: "not running in enclave" }, 503);
    }
    return c.json({ attestation: toHex(doc) });
  });

  app.onError((err, c) => {
    console.error(`[nautilus] ${c.req.method} ${c.req.path} error:`, err);
    return c.json(
      { error: inEnclave ? "internal error" : (err.message ?? "internal error") },
      500,
    );
  });

  app.notFound((c) => c.json({ error: "not found" }, 404));

  return { app, ctx };
}
