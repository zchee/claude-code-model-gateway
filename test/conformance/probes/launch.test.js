// Copyright 2026 The claude-code-model-gateway Authors.
// SPDX-License-Identifier: Apache-2.0
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const { gatewayTestEnvironment, startGateway, stopGatewayChild, requestFixtureWorkerDrain } =
  require(path.resolve('test/support.js'));

test('launch probe: stdout listener, health pid and fixture IPC shutdown', async t => {
  const env = gatewayTestEnvironment(t);
  // startGateway resolves only after observing the listening line and then a
  // complete 200 response. In particular a child that only opens a port fails.
  const { child, port } = await startGateway(t, 'serve-shim', env);
  assert.equal(child.spawnargs.at(-1), 'serve-shim');
  const response = await fetch(`http://127.0.0.1:${port}/healthz`);
  assert.equal(response.status, 200);
  assert.equal((await response.json()).supervisorPid, child.pid);
  assert.equal(child.connected, true);
  assert.equal(child.fixtureShutdown, true);
  assert.equal(await stopGatewayChild(child), true);
  assert.equal(child.signalCode, null);
  assert.equal(child.exitCode, 0);
  t.after(() => assert.equal(fs.existsSync(env.HOME), false));
});

test('launch probe: standalone worker accepts fixture drain', async t => {
  const env = gatewayTestEnvironment(t);
  const { child } = await startGateway(t, 'serve-worker', env);
  assert.equal(child.spawnargs.at(-1), 'serve-worker');
  // The harness reads its own fixture token, requires HTTP 202 and validates
  // the response body. A delivered signal or a closed socket cannot pass.
  assert.deepEqual(await requestFixtureWorkerDrain(child, Date.now() + 5000), { kind: 'accepted', code: 'accepted' });
  assert.equal(await stopGatewayChild(child), true);
  assert.equal(child.exitCode, 0);
  t.after(() => assert.equal(fs.existsSync(env.HOME), false));
});

test('launch probe: copied proxy is real Node and fixture NODE_OPTIONS survives', async t => {
  const env = gatewayTestEnvironment(t);
  const proxy = path.join(env.HOME, 'claude-code-proxy');
  fs.copyFileSync(process.execPath, proxy);
  const preload = path.join(env.HOME, 'proxy-preload.cjs');
  fs.writeFileSync(preload, "process.env.PROBE_PRELOAD_VALUE = 'loaded';\n");
  env.NODE_OPTIONS = `--require=${JSON.stringify(preload)}`;
  const result = spawnSync(proxy, ['-p', 'process.env.PROBE_PRELOAD_VALUE'], { env, encoding: 'utf8' });
  assert.equal(result.status, 0);
  assert.equal(result.stdout.trim(), 'loaded');
  const { child } = await startGateway(t, 'serve-worker', env);
  if (process.platform === 'linux') {
    const args = fs.readFileSync(`/proc/${child.pid}/cmdline`, 'utf8').split('\0');
    assert.equal(args.at(-2), 'serve-worker');
    const fields = fs.readFileSync(`/proc/${child.pid}/environ`, 'utf8').split('\0');
    for (const key of ['HOME', 'CLAUDE_CONFIG_DIR', 'CODEX_GATEWAY_PORT', 'CODEX_GATEWAY_WORKER_PORT', 'NODE_OPTIONS']) {
      assert.ok(fields.includes(`${key}=${env[key]}`), `child received fixture ${key}`);
    }
  }
  assert.equal(await stopGatewayChild(child), true);
  assert.equal(child.exitCode, 0);
  t.after(() => assert.equal(fs.existsSync(env.HOME), false));
});
