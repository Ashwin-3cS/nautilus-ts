/**
 * Tests for boot configuration parsing and dev config.
 */

import { describe, test, expect, beforeAll, afterAll } from "bun:test";
import { devBootConfig } from "../src/core/config.ts";
import type { BootConfig } from "../src/core/config.ts";

describe("devBootConfig", () => {
  test("returns defaults when no path given", async () => {
    const config = await devBootConfig();
    expect(config.endpoints).toEqual([]);
    expect(config.log_level).toBeDefined();
    expect(config.app).toEqual({});
  });

  test("reads config from file", async () => {
    const tmpPath = "/tmp/nautilus-test-config.json";
    const testConfig: BootConfig = {
      endpoints: [
        { host: "example.com", vsock_port: 8443 },
      ],
      secrets: { API_KEY: "test-key" },
      log_level: "debug",
      app: { foo: "bar" },
    };

    await Bun.write(tmpPath, JSON.stringify(testConfig));
    const config = await devBootConfig(tmpPath);

    expect(config.endpoints).toHaveLength(1);
    expect(config.endpoints[0].host).toBe("example.com");
    expect(config.endpoints[0].vsock_port).toBe(8443);
    expect(config.secrets?.API_KEY).toBe("test-key");
    expect(config.log_level).toBe("debug");
    expect(config.app?.foo).toBe("bar");
  });

  test("throws on invalid JSON", async () => {
    const tmpPath = "/tmp/nautilus-test-bad-config.json";
    await Bun.write(tmpPath, "not valid json {{{");

    expect(devBootConfig(tmpPath)).rejects.toThrow();
  });

  test("throws on nonexistent file", async () => {
    expect(devBootConfig("/tmp/nautilus-nonexistent.json")).rejects.toThrow();
  });

  test("respects LOG_LEVEL env var", async () => {
    const prev = process.env.LOG_LEVEL;
    process.env.LOG_LEVEL = "warn";

    const config = await devBootConfig();
    expect(config.log_level).toBe("warn");

    if (prev !== undefined) process.env.LOG_LEVEL = prev;
    else delete process.env.LOG_LEVEL;
  });
});

describe("BootConfig shape", () => {
  test("minimal config is valid", async () => {
    const tmpPath = "/tmp/nautilus-test-minimal.json";
    await Bun.write(tmpPath, JSON.stringify({ endpoints: [] }));
    const config = await devBootConfig(tmpPath);

    expect(config.endpoints).toEqual([]);
    expect(config.secrets).toBeUndefined();
    expect(config.log_level).toBeUndefined();
    expect(config.app).toBeUndefined();
  });

  test("config with multiple endpoints", async () => {
    const tmpPath = "/tmp/nautilus-test-multi.json";
    await Bun.write(tmpPath, JSON.stringify({
      endpoints: [
        { host: "sui.io", vsock_port: 8001 },
        { host: "walrus.io", vsock_port: 8002 },
        { host: "seal.io", vsock_port: 8003 },
      ],
    }));
    const config = await devBootConfig(tmpPath);
    expect(config.endpoints).toHaveLength(3);
  });
});
