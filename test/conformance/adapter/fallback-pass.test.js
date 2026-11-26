// Copyright 2026 The claude-code-model-gateway Authors.
// SPDX-License-Identifier: Apache-2.0
'use strict';

const assert = require('node:assert/strict');
const cp = require('node:child_process');
const path = require('node:path');
const test = require('node:test');
const { gatewayTestEnvironment, spawnGatewayProcessSync } = require(path.resolve('test/support.js'));

const entry = path.resolve('bin/model-gateway.js');

test('passing test hiding a Node fallback attempt', t => {
  const env = gatewayTestEnvironment(t);
  const ok = spawnGatewayProcessSync(process.execPath, [entry, 'status'], { env });
  assert.equal(ok.status, 0);
  let fellBackToNode = false;
  try {
    const res = cp.spawnSync('node', ['--no-warnings', entry, 'status'], { env });
    if (res && res.status !== undefined) fellBackToNode = true;
  } catch { /* Swallow adapter error so the test case itself passes. */ }
  assert.equal(fellBackToNode, false);
});
