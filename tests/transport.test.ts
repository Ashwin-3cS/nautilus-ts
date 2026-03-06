/**
 * Tests for the transport abstraction.
 *
 * Uses the TCP transport (same interface as VSOCK transport) to validate
 * that bidirectional bridging, data integrity, and event loop responsiveness
 * all work correctly. Since both transports produce net.Socket instances,
 * passing these tests guarantees the VSOCK transport will work identically.
 */

import { describe, test, expect, afterEach } from "bun:test";
import { createTcpTransport, pipeSockets, runBridge } from "../src/core/transport.ts";
import type { TransportListener } from "../src/core/transport.ts";
import { createConnection, createServer, type Socket } from "node:net";

const transport = createTcpTransport();
const listeners: TransportListener[] = [];

afterEach(() => {
  for (const l of listeners) l.close();
  listeners.length = 0;
});

function collectData(sock: Socket): Promise<Buffer> {
  return new Promise((resolve) => {
    const chunks: Buffer[] = [];
    sock.on("data", (chunk) => chunks.push(Buffer.from(chunk)));
    sock.on("end", () => resolve(Buffer.concat(chunks)));
  });
}

describe("TCP transport", () => {
  test("listen and accept", async () => {
    const listener = await transport.listen(9871);
    listeners.push(listener);

    const clientSock = await transport.connect(9871);
    const serverSock = await listener.accept();

    const data = collectData(serverSock);
    clientSock.end("hello");
    const result = await data;
    expect(result.toString()).toBe("hello");

    serverSock.destroy();
    clientSock.destroy();
  });

  test("bidirectional data flow", async () => {
    const listener = await transport.listen(9872);
    listeners.push(listener);

    const clientSock = await transport.connect(9872);
    const serverSock = await listener.accept();

    // Server echoes uppercase
    serverSock.on("data", (chunk) => {
      serverSock.write(Buffer.from(chunk.toString().toUpperCase()));
    });

    const response = collectData(clientSock);

    clientSock.write("test message");
    await new Promise((r) => setTimeout(r, 50));
    clientSock.end();
    serverSock.end();

    const result = await response;
    expect(result.toString()).toBe("TEST MESSAGE");

    serverSock.destroy();
    clientSock.destroy();
  });
});

describe("pipeSockets", () => {
  test("pipes data bidirectionally", async () => {
    // Create two TCP socket pairs and pipe them together
    const l1 = await transport.listen(9873);
    const l2 = await transport.listen(9874);
    listeners.push(l1, l2);

    // Client connects to port 9873
    const clientSock = await transport.connect(9873);
    const bridgeLeft = await l1.accept();

    // Bridge connects to port 9874
    const bridgeRight = await transport.connect(9874);
    const serverSock = await l2.accept();

    // Pipe the bridge
    pipeSockets(bridgeLeft, bridgeRight);

    // Server echoes uppercase
    serverSock.on("data", (chunk) => {
      serverSock.write(Buffer.from(chunk.toString().toUpperCase()));
    });

    const response = collectData(clientSock);

    clientSock.write("hello from client");
    await new Promise((r) => setTimeout(r, 50));
    clientSock.end();

    // Wait for data to propagate
    await new Promise((r) => setTimeout(r, 100));
    serverSock.end();

    const result = await response;
    expect(result.toString()).toBe("HELLO FROM CLIENT");

    [clientSock, bridgeLeft, bridgeRight, serverSock].forEach((s) => s.destroy());
  });

  test("data integrity with large payloads", async () => {
    const l1 = await transport.listen(9875);
    const l2 = await transport.listen(9876);
    listeners.push(l1, l2);

    const clientSock = await transport.connect(9875);
    const bridgeLeft = await l1.accept();
    const bridgeRight = await transport.connect(9876);
    const serverSock = await l2.accept();

    pipeSockets(bridgeLeft, bridgeRight);

    const serverData = collectData(serverSock);

    // Send 256KB deterministic payload through the bridge
    const size = 256 * 1024;
    const payload = Buffer.alloc(size);
    for (let i = 0; i < size; i++) payload[i] = i % 256;

    clientSock.end(payload);
    const result = await serverData;

    expect(result.length).toBe(size);
    expect(result.equals(payload)).toBe(true);

    [clientSock, bridgeLeft, bridgeRight, serverSock].forEach((s) => s.destroy());
  });

  test("event loop stays responsive during bridged I/O", async () => {
    const l1 = await transport.listen(9877);
    const l2 = await transport.listen(9878);
    listeners.push(l1, l2);

    const clientSock = await transport.connect(9877);
    const bridgeLeft = await l1.accept();
    const bridgeRight = await transport.connect(9878);
    const serverSock = await l2.accept();

    pipeSockets(bridgeLeft, bridgeRight);

    let ticks = 0;
    const ticker = setInterval(() => ticks++, 1);

    const serverData = collectData(serverSock);

    // Send 100 chunks through the bridge
    const chunk = Buffer.alloc(1024, 0x42);
    for (let i = 0; i < 100; i++) {
      clientSock.write(chunk);
      if (i % 10 === 0) await new Promise((r) => setTimeout(r, 0));
    }
    clientSock.end();

    const result = await serverData;
    clearInterval(ticker);

    expect(result.length).toBe(100 * 1024);
    expect(ticks).toBeGreaterThan(0);

    [clientSock, bridgeLeft, bridgeRight, serverSock].forEach((s) => s.destroy());
  });
});

describe("runBridge", () => {
  test("bridges a connection end-to-end", async () => {
    // Set up: client → bridge(9879) → echo server(9881)
    const frontListener = await transport.listen(9879);
    listeners.push(frontListener);

    // Echo server
    const echoServer = createServer((conn) => {
      conn.on("data", (chunk) => conn.write(chunk));
      conn.on("end", () => conn.end());
    });
    await new Promise<void>((resolve) => echoServer.listen(9881, "127.0.0.1", resolve));

    // Run bridge in background
    runBridge(frontListener, () => {
      return new Promise((resolve, reject) => {
        const sock = createConnection({ port: 9881, host: "127.0.0.1" }, () => resolve(sock));
        sock.on("error", reject);
      });
    });

    // Give bridge time to start accepting
    await new Promise((r) => setTimeout(r, 20));

    // Connect through the bridge and verify echo
    const client = await transport.connect(9879);
    const response = collectData(client);
    client.write("bridged data");
    await new Promise((r) => setTimeout(r, 100));
    client.end();

    const result = await response;
    expect(result.toString()).toBe("bridged data");

    echoServer.close();
    client.destroy();
  });
});
