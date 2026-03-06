/**
 * Enclave networking setup.
 *
 * Inside a Nitro Enclave there is no network interface — only VSOCK.
 * To let the application make normal HTTPS requests (e.g. to Sui RPC,
 * Seal key servers, Walrus aggregators), we:
 *
 * 1. Write /etc/hosts entries mapping each endpoint hostname to a
 *    loopback address (127.0.0.64, 127.0.0.65, ...).
 * 2. Start a TCP→VSOCK traffic forwarder on each loopback address:443
 *    that forwards to the parent VM's corresponding VSOCK port.
 * 3. The parent VM runs socat (or vsock-proxy) to forward each VSOCK
 *    port to the real internet endpoint.
 *
 * This is transparent to the application — `fetch("https://example.com")`
 * resolves via /etc/hosts → 127.0.0.x → traffic forwarder → VSOCK →
 * parent VM → internet.
 */

import type { Endpoint } from "./config.ts";
import { runBridge } from "./transport.ts";

const PARENT_CID = 3;

/**
 * Set up loopback interface (required inside enclave — no network by default).
 */
export function setupLoopback(): void {
  try {
    Bun.spawnSync(["ip", "addr", "add", "127.0.0.1/8", "dev", "lo"]);
    Bun.spawnSync(["ip", "link", "set", "dev", "lo", "up"]);
    console.log("[net] loopback configured");
  } catch {
    // Outside enclave (dev mode), loopback already exists
  }
}

/**
 * Configure /etc/hosts and start traffic forwarders for all endpoints.
 */
export async function setupEndpoints(endpoints: Endpoint[]): Promise<void> {
  if (endpoints.length === 0) return;

  const { vsockTransportListener, vsockTransportConnect } = await import("./vsock.ts");

  // Build /etc/hosts
  const hostsLines = ["127.0.0.1   localhost"];
  for (let i = 0; i < endpoints.length; i++) {
    const ip = `127.0.0.${64 + i}`;
    hostsLines.push(`${ip}   ${endpoints[i].host}`);
  }

  await Bun.write("/etc/hosts", hostsLines.join("\n") + "\n").catch(() => {
    console.log("[net] skipping /etc/hosts (not in enclave)");
  });

  // Start traffic forwarders using the transport abstraction
  for (let i = 0; i < endpoints.length; i++) {
    const ip = `127.0.0.${64 + i}`;
    const ep = endpoints[i];
    console.log(`[net] ${ep.host} → ${ip}:443 → VSOCK:${PARENT_CID}:${ep.vsock_port}`);

    // Listen on local IP:443 using Bun.listen, bridge to VSOCK
    Bun.listen({
      hostname: ip,
      port: 443,
      socket: {
        open(socket) {
          try {
            const vsockSock = vsockTransportConnect(PARENT_CID, ep.vsock_port);
            (socket.data as any) = { vsockSock };
            vsockSock.on("data", (chunk: Buffer) => socket.write(chunk));
            vsockSock.on("end", () => socket.end());
            vsockSock.on("error", (err: Error) => {
              console.error(`[traffic] vsock error: ${err}`);
              socket.end();
            });
          } catch (e) {
            console.error(`[traffic] vsock connect failed: ${e}`);
            socket.end();
          }
        },
        data(socket, data) {
          const { vsockSock } = (socket.data as any) ?? {};
          if (vsockSock) vsockSock.write(Buffer.from(data));
        },
        close(socket) {
          const { vsockSock } = (socket.data as any) ?? {};
          if (vsockSock) vsockSock.destroy();
        },
        error(_socket, err) {
          console.error(`[traffic] error: ${err}`);
        },
      },
    });
  }
}
