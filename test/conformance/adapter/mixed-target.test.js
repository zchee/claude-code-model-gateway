// Copyright 2026 The claude-code-model-gateway Authors.
// SPDX-License-Identifier: Apache-2.0
'use strict';

const assert = require('node:assert/strict');
const cp = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const { gatewayTestEnvironment, spawnGatewayProcessSync } = require(path.resolve('test/support.js'));

const entry = path.resolve('bin/model-gateway.js');

test('mixed-target control: one redirected launch, decoys on Node, alternate spellings and extra options fail closed', t => {
  const env = gatewayTestEnvironment(t);
  // 1. Inventoried launch through test/support.js redirects to the recording binary.
  const redirected = spawnGatewayProcessSync(process.execPath, [entry, 'status'], { env });
  assert.equal(redirected.status, 0);
  const text = redirected.stdout.toString();
  const end = text.indexOf('\n');
  assert.equal(Number(text.slice(0, end)), 1);
  assert.equal(text.slice(end + 1).split('\0')[0], 'status');

  // 2. Known decoy (foreign bin/model-gateway.js with different bytes) stays on Node.
  const foreignDir = path.join(env.HOME, 'foreign', 'bin');
  fs.mkdirSync(foreignDir, { recursive: true });
  const foreignCli = path.join(foreignDir, 'model-gateway.js');
  fs.writeFileSync(foreignCli, 'process.stdout.write("DECOY_NODE_OK\\n");\n');
  const decoy = cp.spawnSync(process.execPath, [foreignCli, 'serve-shim'], { env, encoding: 'utf8' });
  assert.equal(decoy.status, 0);
  assert.equal(decoy.stdout.trim(), 'DECOY_NODE_OK');

  // 3. Direct product launch outside inventoried sites must fail closed, never falling back to Node.
  assert.throws(
    () => cp.spawnSync(process.execPath, [entry, 'status'], { env }),
    /conformance adapter: product launch \(cli\) outside inventoried sites/,
  );

  // 4. Alternative Node executable spellings must not bypass classification.
  assert.throws(
    () => cp.spawnSync('node', [entry, 'status'], { env }),
    /conformance adapter: product launch \(cli\) outside inventoried sites/,
  );

  const nodeSymlink = path.join(env.HOME, 'alt-node-link');
  fs.symlinkSync(process.execPath, nodeSymlink);
  assert.throws(
    () => cp.spawnSync(nodeSymlink, [entry, 'status'], { env }),
    /conformance adapter: product launch \(cli\) outside inventoried sites/,
  );

  assert.throws(
    () => cp.spawnSync('/usr/bin/env', ['node', entry, 'status'], { env }),
    /conformance adapter: product launch \(cli\) outside inventoried sites/,
  );

  // 5. Extra Node options before the product script must fail closed rather than falling back to Node.
  assert.throws(
    () => cp.spawnSync(process.execPath, ['--no-warnings', entry, 'status'], { env }),
    /conformance adapter: unsupported Node options before product entry/,
  );

  assert.throws(
    () => cp.spawnSync(process.execPath, ['--import', './test/isolate-home.js', entry, 'status'], { env }),
    /conformance adapter: unsupported Node options before product entry/,
  );

  assert.throws(
    () => cp.spawnSync(process.execPath, ['--unknown-node-flag', 'val', entry, 'status'], { env }),
    /conformance adapter: unsupported Node option/,
  );

  // 6. Alternate child_process APIs must not bypass classification.
  assert.throws(
    () => cp.execFileSync(process.execPath, [entry, 'status'], { env }),
    /conformance adapter: unsupported product invocation via child_process\.execFileSync/,
  );
});
