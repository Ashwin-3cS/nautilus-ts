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

export interface BootOptions {
  /** Port the HTTP server will listen on (default: 3000). */
  port?: number;
  /** Path to a local config file for dev mode. */
  devConfigPath?: string;
  /** @internal Override enclave detection for testing. */
  _testAsEnclave?: boolean;
}

export interface BootResult {
  /** Hono app with built-in routes (/health_check, /get_attestation). */
  app: Hono;
  /** Enclave context: signing, attestation, config, crypto utilities. */
  ctx: NautilusContext;
}

/** Spawn the traffic proxy as a child process. */
function startTrafficProxy(config: BootConfig, httpPort: number): void {
  const proxyConfig = JSON.stringify({
    httpVsockPort: httpPort,
    httpTcpPort: httpPort,
    endpoints: config.endpoints,
  });

  const proc = Bun.spawn(["/traffic-proxy", "enclave"], {
    stdin: new Blob([proxyConfig]),
    stdout: "inherit",
    stderr: "inherit",
  });

  proc.exited.then((code) => {
    console.error(`[nautilus] traffic-proxy exited with code ${code}`);
    process.exit(1);
  });

  console.log(`[nautilus] traffic-proxy started (pid ${proc.pid})`);
}

/**
 * Boot the enclave and return a Hono app with context.
 *
 * In enclave mode:
 *   1. Set up loopback networking
 *   2. Receive config from host via VSOCK:7777
 *   3. Spawn traffic proxy (handles /etc/hosts, TCP↔VSOCK bridges)
 *
 * In dev mode:
 *   1. Read config from file or use defaults
 *
 * Returns a Hono app with built-in routes and error handling,
 * plus the NautilusContext for signing and attestation.
 */
export async function boot(options: BootOptions = {}): Promise<BootResult> {
  const port = options.port ?? 3000;
  const inEnclave = options._testAsEnclave || isEnclave();
  let config: BootConfig;

  if (inEnclave && !options._testAsEnclave) {
    console.log("[nautilus] booting in enclave mode");
    setupLoopback();
    config = await receiveBootConfig();

    // Spawn traffic proxy — handles /etc/hosts, inbound + outbound bridges
    startTrafficProxy(config, port);
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

  // Create Hono app with built-in routes and error handling
  const app = new Hono();

  app.get("/", (c) => c.text("Pong!"));

  app.get("/health_check", (c) =>
    c.json({ pk: ctx.publicKey, address: ctx.address }),
  );

  app.get("/get_attestation", async (c) => {
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
