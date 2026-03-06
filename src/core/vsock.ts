/**
 * VSOCK socket support via libc FFI.
 *
 * Uses bun:ffi for VSOCK-specific syscalls (socket, bind, listen, accept, connect)
 * since Bun/Node don't natively support AF_VSOCK. Once we have a connected fd,
 * we wrap it with net.Socket({ fd }) for async, non-blocking I/O.
 *
 * Higher-level bridging uses the Transport abstraction from transport.ts.
 *
 * AF_VSOCK constants (Linux):
 *   AF_VSOCK        = 40
 *   VMADDR_CID_ANY  = 0xFFFFFFFF  (bind to any CID)
 *   VMADDR_CID_HOST = 2
 *   VMADDR_CID_PARENT = 3         (the EC2 host VM)
 */

import { dlopen, FFIType, ptr } from "bun:ffi";
import { Socket } from "node:net";
import type { TransportListener } from "./transport.ts";

const AF_VSOCK = 40;
const SOCK_STREAM = 1;
const VMADDR_CID_ANY = 0xffffffff;
const SOL_SOCKET = 1;
const SO_REUSEADDR = 2;

// sockaddr_vm is 16 bytes: { family: u16, reserved1: u16, port: u32, cid: u32, flags: u8, zero: [u8; 3] }
const SOCKADDR_VM_SIZE = 16;

// libc FFI — only VSOCK-specific syscalls. Once we have an fd, we wrap it
// in net.Socket({ fd }) for async I/O (no libc read/write needed).
//
// fd parameters use FFIType.i32 (matching C int), not FFIType.ptr.
const libc = dlopen("libc.so.6", {
  socket: { returns: FFIType.i32, args: [FFIType.i32, FFIType.i32, FFIType.i32] },
  bind: { returns: FFIType.i32, args: [FFIType.i32, FFIType.ptr, FFIType.i32] },
  listen: { returns: FFIType.i32, args: [FFIType.i32, FFIType.i32] },
  accept: { returns: FFIType.i32, args: [FFIType.i32, FFIType.ptr, FFIType.ptr] },
  connect: { returns: FFIType.i32, args: [FFIType.i32, FFIType.ptr, FFIType.i32] },
  close: { returns: FFIType.i32, args: [FFIType.i32] },
  setsockopt: {
    returns: FFIType.i32,
    args: [FFIType.i32, FFIType.i32, FFIType.i32, FFIType.ptr, FFIType.i32],
  },
});

function makeSockaddrVm(cid: number, port: number): Buffer {
  const buf = Buffer.alloc(SOCKADDR_VM_SIZE);
  buf.writeUInt16LE(AF_VSOCK, 0); // family
  buf.writeUInt16LE(0, 2); // reserved
  buf.writeUInt32LE(port, 4); // port
  buf.writeUInt32LE(cid, 8); // cid
  buf.writeUInt32LE(0, 12); // flags + zero
  return buf;
}

/** Create a VSOCK socket, bind, and listen. Returns the raw fd. */
export function vsockListen(port: number, cid = VMADDR_CID_ANY, backlog = 5): number {
  const fd = libc.symbols.socket(AF_VSOCK, SOCK_STREAM, 0) as number;
  if (fd < 0) throw new Error(`socket(AF_VSOCK) failed: fd=${fd}`);

  const one = Buffer.alloc(4);
  one.writeInt32LE(1);
  libc.symbols.setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, ptr(one), 4);

  const addr = makeSockaddrVm(cid, port);
  const rc = libc.symbols.bind(fd, ptr(addr), SOCKADDR_VM_SIZE) as number;
  if (rc < 0) {
    libc.symbols.close(fd);
    throw new Error(`bind(VSOCK:${port}) failed: rc=${rc}`);
  }

  const lrc = libc.symbols.listen(fd, backlog) as number;
  if (lrc < 0) {
    libc.symbols.close(fd);
    throw new Error(`listen(VSOCK:${port}) failed: rc=${lrc}`);
  }

  return fd;
}

/** Accept a connection on a VSOCK listener fd. Returns the client fd. */
export function vsockAccept(listenFd: number): number {
  const peerAddr = Buffer.alloc(SOCKADDR_VM_SIZE);
  const addrLen = Buffer.alloc(4);
  addrLen.writeInt32LE(SOCKADDR_VM_SIZE);
  const clientFd = libc.symbols.accept(listenFd, ptr(peerAddr), ptr(addrLen)) as number;
  if (clientFd < 0) throw new Error(`accept() failed: fd=${clientFd}`);
  return clientFd;
}

/** Connect to a VSOCK endpoint. Returns the fd. */
export function vsockConnect(cid: number, port: number): number {
  const fd = libc.symbols.socket(AF_VSOCK, SOCK_STREAM, 0) as number;
  if (fd < 0) throw new Error(`socket(AF_VSOCK) failed: fd=${fd}`);

  const addr = makeSockaddrVm(cid, port);
  const rc = libc.symbols.connect(fd, ptr(addr), SOCKADDR_VM_SIZE) as number;
  if (rc < 0) {
    libc.symbols.close(fd);
    throw new Error(`connect(VSOCK cid=${cid} port=${port}) failed: rc=${rc}`);
  }

  return fd;
}

/** Wrap a raw VSOCK fd into an async net.Socket. */
function wrapFd(fd: number): Socket {
  return new Socket({ fd, readable: true, writable: true });
}

/** Read all data from a VSOCK fd until EOF. */
export function vsockReadAll(fd: number): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const sock = wrapFd(fd);
    const chunks: Buffer[] = [];
    sock.on("data", (chunk) => chunks.push(Buffer.from(chunk)));
    sock.on("end", () => resolve(Buffer.concat(chunks)));
    sock.on("error", reject);
  });
}

/** Close a VSOCK fd. */
export function vsockClose(fd: number): void {
  libc.symbols.close(fd);
}

/**
 * Create a VSOCK TransportListener.
 * Implements the same interface as TCP transport but uses VSOCK underneath.
 */
export function vsockTransportListener(port: number): TransportListener {
  const listenFd = vsockListen(port);
  return {
    accept(): Promise<Socket> {
      const clientFd = vsockAccept(listenFd);
      return Promise.resolve(wrapFd(clientFd));
    },
    close() {
      vsockClose(listenFd);
    },
  };
}

/** Connect to a VSOCK endpoint, returning an async net.Socket. */
export function vsockTransportConnect(cid: number, port: number): Socket {
  const fd = vsockConnect(cid, port);
  return wrapFd(fd);
}
