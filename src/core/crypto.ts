/**
 * Cryptographic utilities for Nautilus enclaves.
 *
 * Uses @noble/ed25519 and @noble/hashes — no Rust FFI needed for signing.
 * Only NSM attestation requires Rust (see nsm/ module).
 */

import * as ed from "@noble/ed25519";
import { sha512 } from "@noble/hashes/sha2";
import { blake2b } from "@noble/hashes/blake2b";
import { sha256 } from "@noble/hashes/sha2";

// @noble/ed25519 v2 requires sha512 to be configured
ed.etc.sha512Sync = (...m: Uint8Array[]) => {
  const h = sha512.create();
  for (const msg of m) h.update(msg);
  return h.digest();
};

export interface Keypair {
  privateKey: Uint8Array;
  publicKey: Uint8Array;
}

/**
 * Generate a new Ed25519 keypair.
 * Mixes NSM hardware entropy (if available) with crypto.getRandomValues()
 * so that neither source alone can determine the key.
 */
export function generateKeypair(nsmEntropy?: Uint8Array | null): Keypair {
  const osRandom = ed.utils.randomPrivateKey(); // 32 bytes from crypto.getRandomValues()
  let privateKey: Uint8Array;
  if (nsmEntropy && nsmEntropy.length >= 32) {
    // XOR: both sources must be compromised to predict the key
    privateKey = new Uint8Array(32);
    for (let i = 0; i < 32; i++) privateKey[i] = osRandom[i] ^ nsmEntropy[i];
  } else {
    privateKey = osRandom;
  }
  const publicKey = ed.getPublicKey(privateKey);
  return { privateKey, publicKey };
}

/** Sign a message with Ed25519. Returns 64-byte signature. */
export function sign(keypair: Keypair, message: Uint8Array): Uint8Array {
  return ed.sign(message, keypair.privateKey);
}

/** Verify an Ed25519 signature. */
export function verify(
  publicKey: Uint8Array,
  message: Uint8Array,
  signature: Uint8Array,
): boolean {
  return ed.verify(signature, message, publicKey);
}

/**
 * Derive Sui address from Ed25519 public key.
 * Sui address = blake2b256(0x00 || public_key)[0..32]
 */
export function suiAddress(publicKey: Uint8Array): string {
  const input = new Uint8Array(1 + publicKey.length);
  input[0] = 0x00; // Ed25519 flag byte
  input.set(publicKey, 1);
  const hash = blake2b(input, { dkLen: 32 });
  return "0x" + Buffer.from(hash).toString("hex");
}

/** Hex encode bytes. */
export function toHex(bytes: Uint8Array): string {
  return Buffer.from(bytes).toString("hex");
}

/** Hex decode (handles 0x prefix). Throws on invalid input. */
export function fromHex(hex: string): Uint8Array {
  const s = hex.startsWith("0x") ? hex.slice(2) : hex;
  if (s.length % 2 !== 0) throw new Error("fromHex: odd-length input");
  if (s.length > 0 && !/^[0-9a-fA-F]+$/.test(s)) throw new Error("fromHex: invalid hex characters");
  return new Uint8Array(Buffer.from(s, "hex"));
}

/** Blake2b-256 hash. */
export function blake2b256(data: Uint8Array): Uint8Array {
  return blake2b(data, { dkLen: 32 });
}

/** SHA-256 hash. */
export function sha256Hash(data: Uint8Array): Uint8Array {
  return sha256(data);
}
