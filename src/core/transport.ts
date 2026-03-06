/**
 * Transport abstraction for socket I/O.
 *
 * Decouples the bridge logic from the underlying socket type (VSOCK vs TCP).
 * In production: VSOCK via libc FFI → net.Socket({ fd })
 * In testing:    TCP socket pairs via node:net
 *
 * The bridge only cares about readable/writable streams — it doesn't need
 * to know whether the underlying transport is VSOCK or TCP.
 */

import { Socket, createConnection, createServer } from "node:net";

export interface Transport {
  /** Create a listener. Returns an object that can accept connections. */
  listen(port: number): Promise<TransportListener>;
  /** Connect to a remote endpoint. Returns a connected socket. */
  connect(port: number, host?: number | string): Promise<Socket>;
}

export interface TransportListener {
  /** Accept the next incoming connection. */
  accept(): Promise<Socket>;
  /** Close the listener. */
  close(): void;
}

/**
 * VSOCK transport — uses libc FFI for socket/bind/listen/accept/connect,
 * then wraps the resulting fd in net.Socket for async I/O.
 */
export function createVsockTransport(): Transport {
  // Lazy import to avoid loading FFI outside enclave
  let vsockMod: typeof import("./vsock.ts") | null = null;
  async function getVsock() {
    if (!vsockMod) vsockMod = await import("./vsock.ts");
    return vsockMod;
  }

  return {
    async listen(port: number): Promise<TransportListener> {
      const v = await getVsock();
      const listenFd = v.vsockListen(port);
      return {
        accept(): Promise<Socket> {
          // accept() is a blocking FFI call — wrap result in net.Socket
          const clientFd = v.vsockAccept(listenFd);
          return Promise.resolve(new Socket({ fd: clientFd, readable: true, writable: true }));
        },
        close() {
          v.vsockClose(listenFd);
        },
      };
    },
    async connect(port: number, cid?: number | string): Promise<Socket> {
      const v = await getVsock();
      const fd = v.vsockConnect(Number(cid ?? 3), port);
      return new Socket({ fd, readable: true, writable: true });
    },
  };
}

/**
 * TCP transport — uses standard node:net for both sides.
 * Used for local development and testing.
 */
export function createTcpTransport(host = "127.0.0.1"): Transport {
  return {
    async listen(port: number): Promise<TransportListener> {
      const server = createServer();
      const pending: Socket[] = [];
      const waiters: ((sock: Socket) => void)[] = [];

      server.on("connection", (sock) => {
        const waiter = waiters.shift();
        if (waiter) waiter(sock);
        else pending.push(sock);
      });

      await new Promise<void>((resolve) => {
        server.listen(port, host, resolve);
      });

      return {
        accept(): Promise<Socket> {
          const sock = pending.shift();
          if (sock) return Promise.resolve(sock);
          return new Promise((resolve) => waiters.push(resolve));
        },
        close() {
          server.close();
        },
      };
    },
    async connect(port: number, _host?: number | string): Promise<Socket> {
      return new Promise((resolve, reject) => {
        const sock = createConnection({ port, host }, () => resolve(sock));
        sock.on("error", reject);
      });
    },
  };
}

/**
 * Pipe two sockets together bidirectionally.
 * This is the core bridge operation — transport-agnostic.
 * Uses data events instead of .pipe() for reliability with async setup.
 */
export function pipeSockets(a: Socket, b: Socket): void {
  a.on("data", (chunk) => b.write(chunk));
  b.on("data", (chunk) => a.write(chunk));
  a.on("end", () => b.end());
  b.on("end", () => a.end());
  a.on("error", () => b.destroy());
  b.on("error", () => a.destroy());
}

/**
 * Run a bridge: accept connections on a listener and pipe each
 * to a new outbound connection. Pauses inbound until outbound is ready
 * to ensure no data is lost during async connection setup.
 */
export async function runBridge(
  listener: TransportListener,
  connectFn: () => Promise<Socket>,
): Promise<void> {
  while (true) {
    const inbound = await listener.accept();
    // Pause inbound to buffer data while we connect outbound
    inbound.pause();
    connectFn()
      .then((outbound) => {
        pipeSockets(inbound, outbound);
        inbound.resume();
      })
      .catch((e) => {
        console.error(`[bridge] connect failed: ${e}`);
        inbound.destroy();
      });
  }
}
