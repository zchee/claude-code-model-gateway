// Copyright 2026 The claude-code-model-gateway Authors.
// SPDX-License-Identifier: Apache-2.0
'use strict';

const assert = require('node:assert/strict');
const cp = require('node:child_process');
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const [root, plugin, work, target, mode, phase, pkg, gate, log, classification] = process.argv.slice(2);
const data = path.join(root, 'test/conformance');
const tsv = file => {
  const [header, ...rows] = fs.readFileSync(path.join(data, file), 'utf8').trimEnd().split('\n');
  const keys = header.split('\t');
  const unescape = value => value.replace(/\\([\\tnr])/g, (_, c) => ({ '\\': '\\', t: '\t', n: '\n', r: '\r' })[c]);
  return rows.map(line => Object.fromEntries(line.split('\t').map((value, i) => [keys[i], unescape(value)])));
};
const ndjson = file => fs.readFileSync(file, 'utf8').split('\n').filter(Boolean).map(line => JSON.parse(line));
const key = r => [r.path, r.line, r.column, r.name].join('\t');
const sha256 = file => crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex');
const identity = target === 'identity';
const binary = target.slice('binary:'.length);
if (!identity) {
  assert(target.startsWith('binary:') && path.isAbsolute(binary) && fs.realpathSync(binary) === binary,
    'TARGET must be identity or binary:<absolute canonical executable>');
  fs.accessSync(binary, fs.constants.X_OK);
}
assert(path.isAbsolute(log), 'LAUNCH_LOG must be absolute');
fs.closeSync(fs.openSync(log, 'wx', 0o600));
const config = { root, plugin, target, work, log, build: identity ? null : sha256(binary),
  witness: mode === 'self-test' ? path.join(work, 'standin') : null };
const preload = path.join(work, 'preload.cjs');
fs.writeFileSync(preload, `require(${JSON.stringify(path.join(__dirname, 'preload.cjs'))})(${JSON.stringify(config)});\n`);

const baseline = tsv('baseline/census.tsv');
const classes = tsv('classification.tsv');
const probes = mode === 'probes';
assert(!classification || classification === 'conformance', 'CLASS must be conformance');
assert(!probes || !(phase || pkg || gate || classification), 'probe mode does not accept case selectors');
const full = !probes && identity && !(phase || pkg || gate || classification);
assert(mode !== 'self-test' || full, 'self-test requires unfiltered identity');
const signalCase = 'supervisor records an orderly signal before it exits';
const selected = classes.filter(r => probes ? r.name === signalCase :
  full || (r.class === 'conformance' && (!phase || r.phase === phase) && (!pkg || r.package === pkg) && (!gate || r.gate === gate)));
assert(selected.length, 'empty conformance selection');
const escapeRE = s => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
const files = [...new Set(selected.map(r => r.path))];
const pattern = selected.map(r => escapeRE(r.name.replaceAll(' > ', ' ')));
if (probes) {
  files.push(path.join(data, 'probes/launch.test.js'));
  pattern.push('launch probe: .*');
}
const manifest = require(path.join(plugin, 'package.json')).scripts.test;
assert.equal(manifest, 'node --require ./test/isolate-home.js --test --test-concurrency=2 --test-timeout=300000 "test/*.test.js"', 'unexpected pinned test command');
const eventsFile = path.join(work, 'events.jsonl');
const result = cp.spawnSync(process.execPath, ['--require', './test/isolate-home.js', '--require', preload,
  '--test', '--test-concurrency=2', '--test-timeout=300000', '--test-reporter=tap', '--test-reporter-destination=stdout',
  `--test-reporter=${path.join(root, 'hack/baseline-node-reporter.mjs')}`, `--test-reporter-destination=${eventsFile}`,
  ...full ? [] : [`--test-name-pattern=^(?:${pattern.join('|')})$`], ...files], { cwd: plugin, stdio: 'inherit' });
if (result.error) throw result.error;
let failed = result.status !== 0;
function error(message) { failed = true; process.stderr.write(`conformance: ${message}\n`); }
const launches = ndjson(log);
for (const record of launches) process.stdout.write(`# launch ${JSON.stringify(record)}\n`);
const sites = tsv('launch-sites.tsv');
const observed = new Map();
const stacks = new Map();
for (const event of ndjson(eventsFile)) {
  if (!['test:start', 'test:pass', 'test:fail'].includes(event.type)) continue;
  const file = event.file.startsWith('..') ? path.relative(root, path.resolve(plugin, event.file)) : event.file;
  const stack = stacks.get(file) ?? [];
  if (event.type === 'test:start') {
    stack[event.nesting] = event.name;
    stacks.set(file, stack);
    continue;
  }
  const name = stack.slice(0, event.nesting + 1).join(' > ');
  const id = key({ ...event, path: file, name });
  if (observed.has(id)) error(`duplicate result ${id}`);
  observed.set(id, event.skip ? 'skip' : event.todo ? 'todo' : event.type === 'test:pass' ? 'pass' : 'fail');
}
const statuses = new Map(baseline.map(r => [key(r), r.status]));
const selectedKeys = new Set(selected.map(key));
for (const r of selected) {
  const id = key(r);
  const expected = full ? statuses.get(id) : 'pass';
  if (observed.get(id) !== expected) error(`${id}: expected ${expected}, got ${observed.get(id) ?? 'missing'}`);
  if (observed.get(id) !== 'pass') continue;
  const obligations = sites.filter(s => s.path === r.path && s.case === r.name && s.kind !== 'helper');
  if (r.class !== 'conformance' && !obligations.length) continue;
  // Inventory container rows explicitly name the nested case whose launch
  // exercises the parent; never give a sibling or arbitrary parent credit.
  const delegated = obligations.filter(s => s.via.startsWith('subtest: '));
  if (!launches.some(l => key(l) === id || delegated.some(s => l.path === r.path &&
    l.name === `${r.name} > ${s.via.slice(9)}` && l.site === `${s.path}:${s.line}`))) error(`no attributed launch: ${id}`);
}
for (const [id, status] of observed) {
  if (selectedKeys.has(id)) continue;
  if (probes && id.startsWith('test/conformance/probes/launch.test.js\t')) {
    if (status !== 'pass' || !launches.some(l => key(l) === id)) error(`unproven probe: ${id}`);
  } else if (full) error(`unexpected registration: ${id}`);
}
if (probes && ![...observed.keys()].some(id => id.startsWith('test/conformance/probes/launch.test.js\t'))) error('probe file produced no tests');
if (!identity && config.build !== sha256(binary)) error('target binary changed during the run');
if (mode === 'self-test') {
  for (const s of sites) {
    if (s.kind === 'helper' || s.case === '-' || s.via.startsWith('subtest: ')) continue;
    const site = `${s.path}:${s.line}`;
    if (!launches.some(l => l.path === s.path && l.name === s.case && l.kind === s.kind && l.witnessPid > 0 &&
        (l.site === site || l.callers?.includes(site)))) {
      error(`no recording binary at ${s.path}:${s.line} for ${s.case}`);
    }
  }
  const binaryConfig = { ...config, target: `binary:${config.witness}`, build: sha256(config.witness),
    log: path.join(work, 'standin.jsonl'), witness: null };
  fs.writeFileSync(preload, `require(${JSON.stringify(path.join(__dirname, 'preload.cjs'))})(${JSON.stringify(binaryConfig)});\n`);
  const check = cp.spawnSync(process.execPath, ['--require', './test/isolate-home.js', '--require', preload,
    '--test', '--test-reporter=tap', path.join(__dirname, 'dispatch.test.js')], { cwd: plugin, encoding: 'utf8' });
  if (check.status !== 0) error(`stand-in dispatch failed:\n${check.stdout}\n${check.stderr}`);
  const records = ndjson(binaryConfig.log);
  if (records.length !== 2 || records.some(r => r.target !== binaryConfig.target || r.build !== binaryConfig.build) ||
      new Set(records.map(r => r.pid)).size !== 2) error('missing independent sync/async binary launches');
  for (const r of records) process.stdout.write(`# stand-in ${JSON.stringify(r)}\n`);
}
const counts = Object.fromEntries(['pass', 'fail', 'skip', 'todo'].map(s => [s, [...observed.values()].filter(v => v === s).length]));
process.stdout.write(`# conformance ${JSON.stringify({ target, mode, selected: selected.length, counts, launches: launches.length, ok: !failed })}\n`);
process.exitCode = failed ? 1 : 0;
