import assert from "node:assert/strict";
import test from "node:test";

import {
  DEFAULT_EGRESS_PORT,
  DEFAULT_EXECD_PORT,
  DEFAULT_TIMEOUT_SECONDS,
  Sandbox,
} from "../dist/index.js";

function createAdapterFactory() {
  const recordedRequests = [];
  const endpointCalls = [];
  const egressStackCalls = [];
  const egressService = {
    async getPolicy() {
      return {
        defaultAction: "deny",
        egress: [{ action: "allow", target: "pypi.org" }],
      };
    },
    async patchRules() {},
  };
  const sandboxes = {
    async createSandbox(req) {
      recordedRequests.push(req);
      return { id: "sandbox-test-id", expiresAt: null };
    },
    async getSandbox() {
      throw new Error("not implemented");
    },
    async listSandboxes() {
      throw new Error("not implemented");
    },
    async deleteSandbox() {},
    async pauseSandbox() {},
    async resumeSandbox() {},
    async renewSandboxExpiration() {
      throw new Error("not implemented");
    },
    async getSandboxEndpoint(_sandboxId, port) {
      endpointCalls.push(port);
      return { endpoint: `127.0.0.1:${port}`, headers: { "x-port": String(port) } };
    },
  };

  const adapterFactory = {
    createLifecycleStack() {
      return { sandboxes };
    },
    createExecdStack() {
      return {
        commands: {},
        files: {},
        health: {},
        metrics: {},
      };
    },
    createEgressStack(opts) {
      egressStackCalls.push(opts);
      return { egress: egressService };
    },
  };

  return { adapterFactory, recordedRequests, endpointCalls, egressStackCalls };
}

test("Sandbox.create omits timeout when timeoutSeconds is null", async () => {
  const { adapterFactory, recordedRequests } = createAdapterFactory();

  await Sandbox.create({
    adapterFactory,
    connectionConfig: { domain: "http://127.0.0.1:8080" },
    image: "python:3.12",
    timeoutSeconds: null,
    skipHealthCheck: true,
  });

  assert.equal(recordedRequests.length, 1);
  assert.ok(!Object.hasOwn(recordedRequests[0], "timeout"));
});

test("Sandbox.create floors finite timeoutSeconds", async () => {
  const { adapterFactory, recordedRequests } = createAdapterFactory();

  await Sandbox.create({
    adapterFactory,
    connectionConfig: { domain: "http://127.0.0.1:8080" },
    image: "python:3.12",
    timeoutSeconds: 61.9,
    skipHealthCheck: true,
  });

  assert.equal(recordedRequests.length, 1);
  assert.equal(recordedRequests[0].timeout, 61);
});

test("Sandbox.create uses the default timeout when timeoutSeconds is undefined", async () => {
  const { adapterFactory, recordedRequests } = createAdapterFactory();

  await Sandbox.create({
    adapterFactory,
    connectionConfig: { domain: "http://127.0.0.1:8080" },
    image: "python:3.12",
    skipHealthCheck: true,
  });

  assert.equal(recordedRequests.length, 1);
  assert.equal(recordedRequests[0].timeout, DEFAULT_TIMEOUT_SECONDS);
});

test("Sandbox.create rejects non-finite timeoutSeconds", async () => {
  for (const timeoutSeconds of [Number.NaN, Number.POSITIVE_INFINITY, Number.NEGATIVE_INFINITY]) {
    const { adapterFactory } = createAdapterFactory();
    await assert.rejects(
      Sandbox.create({
        adapterFactory,
        connectionConfig: { domain: "http://127.0.0.1:8080" },
        image: "python:3.12",
        timeoutSeconds,
        skipHealthCheck: true,
      }),
      /timeoutSeconds must be a finite number/
    );
  }
});

test("Sandbox creates and reuses egress service during sandbox lifecycle", async () => {
  const { adapterFactory, endpointCalls, egressStackCalls } = createAdapterFactory();

  const sandbox = await Sandbox.create({
    adapterFactory,
    connectionConfig: { domain: "http://127.0.0.1:8080" },
    image: "python:3.12",
    skipHealthCheck: true,
  });

  await sandbox.getEgressPolicy();
  await sandbox.patchEgressRules([{ action: "allow", target: "www.github.com" }]);

  assert.deepEqual(endpointCalls, [DEFAULT_EXECD_PORT, DEFAULT_EGRESS_PORT]);
  assert.equal(egressStackCalls.length, 1);
  assert.equal(egressStackCalls[0].egressBaseUrl, `http://127.0.0.1:${DEFAULT_EGRESS_PORT}`);
  assert.deepEqual(egressStackCalls[0].endpointHeaders, { "x-port": String(DEFAULT_EGRESS_PORT) });
});
