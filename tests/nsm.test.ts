/**
 * Tests for NSM module.
 *
 * We can't test actual NSM attestation (requires /dev/nsm in a Nitro Enclave),
 * but we can test the detection logic and graceful fallback behavior.
 */

import { describe, test, expect } from "bun:test";
import { isEnclave, getAttestation, getHardwareRandom } from "../src/nsm/index.ts";

describe("isEnclave", () => {
  test("returns false outside enclave", () => {
    // We're running on macOS/Linux dev machine, not inside an enclave
    expect(isEnclave()).toBe(false);
  });

  test("returns a boolean", () => {
    expect(typeof isEnclave()).toBe("boolean");
  });
});

describe("getAttestation", () => {
  test("returns null outside enclave", () => {
    const kp = new Uint8Array(32);
    crypto.getRandomValues(kp);
    expect(getAttestation(kp)).toBeNull();
  });
});

describe("getHardwareRandom", () => {
  test("returns null outside enclave", () => {
    expect(getHardwareRandom()).toBeNull();
  });
});
