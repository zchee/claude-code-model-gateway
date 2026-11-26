// Copyright 2026 The claude-code-model-gateway Authors.
// SPDX-License-Identifier: Apache-2.0
'use strict';

const assert = require('node:assert/strict');
const { once } = require('node:events');
const path = require('node:path');
const test = require('node:test');
const { gatewayTestEnvironment, spawnGatewayProcess, spawnGatewayProcessSync } = require(path.resolve('test/support.js'));
const entry = path.resolve('bin/model-gateway.js');
const args = ['status', '--descriptor', 'two words', '', '✓'];

function checkRecord(bytes, expectIpc) {
  const text = bytes.toString();
  const end = text.indexOf('\n');
  assert.equal(Number(text.slice(0, end)), args.length);
  const fields = text.slice(end + 1).split('\0');
  assert.deepEqual(fields.slice(0, args.length), args);
  const environment = new Set(fields.slice(args.length));
  assert.deepEqual([...environment].filter(s => s.startsWith('NODE_CHANNEL_FD=')), expectIpc ? ['NODE_CHANNEL_FD=3'] : []);
  for (const field of ['ADAPTER_CHECK= x=y ', 'CODEX_GATEWAY_PORT=0', 'NODE_OPTIONS=--no-warnings']) {
    assert.ok(environment.has(field), `missing fixture field ${field}`);
  }
}

test('recording executable receives sync and async fixture launches', async t => {
  const env = gatewayTestEnvironment(t, { ADAPTER_CHECK: ' x=y ', NODE_OPTIONS: '--no-warnings' });
  const sync = spawnGatewayProcessSync(process.execPath, ['bin/model-gateway.js', ...args], { env });
  assert.equal(sync.status, 0);
  checkRecord(sync.stdout, false);
  const child = spawnGatewayProcess(t, process.execPath, [entry, ...args], { env, stdio: ['ignore', 'pipe', 'pipe', 'ipc'] });
  const chunks = [];
  child.stdout.on('data', chunk => chunks.push(chunk));
  assert.deepEqual(await once(child, 'close'), [0, null]);
  checkRecord(Buffer.concat(chunks), true);
});
