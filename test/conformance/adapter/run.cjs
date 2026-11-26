// Copyright 2026 The claude-code-model-gateway Authors.
// SPDX-License-Identifier: Apache-2.0
'use strict';

const assert = require('node:assert/strict');
const cp = require('node:child_process');
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const [root, plugin, work, target, mode, phase, pkg, gate, requestedLog, classification, site] = process.argv.slice(2);
const { createLog, key, parentCredits, tsv, validLaunch } = require('./evidence.cjs');
const data = path.join(root, 'test/conformance');
const inventory = file => tsv(path.join(data, file));
const ndjson = file => fs.readFileSync(file, 'utf8').split('\n').filter(Boolean).map(line => JSON.parse(line));
const sha256 = file => crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex');
const identity = target === 'identity';
const binary = target.slice('binary:'.length);
if (!identity) {
  assert(target.startsWith('binary:') && path.isAbsolute(binary) && fs.realpathSync(binary) === binary,
    'TARGET must be identity or binary:<absolute canonical executable>');
  fs.accessSync(binary, fs.constants.X_OK);
}
const log = createLog(requestedLog, plugin);
const config = { root, plugin, target, work, log, build: identity ? null : sha256(binary),
  witness: mode === 'self-test' ? path.join(work, 'standin') : null };
const preload = path.join(work, 'preload.cjs');
fs.writeFileSync(preload, `require(${JSON.stringify(path.join(__dirname, 'preload.cjs'))})(${JSON.stringify(config)});\n`);

const baseline = inventory('baseline/census.tsv');
const classes = inventory('classification.tsv');
const probes = mode === 'probes';
assert(!classification || classification === 'conformance', 'CLASS must be conformance');
assert(!probes || !(phase || pkg || gate || classification), 'probe mode does not accept case selectors');
assert(!site || (identity && !probes && !(phase || pkg || gate || classification)), 'SITE is an identity diagnostic, not a conformance gate');
const full = !probes && identity && !(site || phase || pkg || gate || classification);
assert(mode !== 'self-test' || full || site, 'self-test requires identity');
const signalCase = 'supervisor records an orderly signal before it exits';
const selected = classes.filter(r => site ? `${r.path}:${r.line}` === site : probes ? r.name === signalCase :
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
let failed = result.status !== 0;
function error(message) { failed = true; process.stderr.write(`conformance: ${message}\n`); }
const launches = ndjson(log);
for (const record of launches) process.stdout.write(`# launch ${JSON.stringify(record)}\n`);
const sites = inventory('launch-sites.tsv');
const parent = parentCredits(sites, baseline);
const credited = new Set();
for (const r of launches) {
  if (!validLaunch(r, target, config.build)) { error(`invalid launch at ${r.site}`); continue; }
  credited.add(key(r));
  if (parent(r)) credited.add(key(parent(r)));
}
const observed = new Map();
const probeKeys = new Set();
const stacks = new Map();
if (result.error) error(result.error.message);
try {
  for (const event of ndjson(eventsFile)) {
    if (['test:abort', 'test:aborted', 'test:cancelled'].includes(event.type) ||
        ['cancelledByParent', 'testAborted', 'testTimeoutFailure'].includes(event.failureType)) error(`aborted test: ${event.name}`);
    if (event.type === 'test:summary' && (event.counts.cancelled || !event.success)) error(`unsuccessful test summary: ${event.file ?? 'suite'}`);
    if (!['test:start', 'test:pass', 'test:fail'].includes(event.type)) continue;
    const file = event.file.startsWith('..') ? path.relative(root, path.resolve(plugin, event.file)) : event.file;
    if (!stacks.has(file)) stacks.set(file, []);
    const stack = stacks.get(file);
    if (event.type === 'test:start') {
      assert.equal(stack.length, event.nesting, `broken parent chain: ${file}:${event.line}`);
      stack.push(event);
      continue;
    }
    assert.equal(stack.length, event.nesting + 1, `unmatched terminal event: ${file}:${event.line}`);
    const start = stack.pop();
    assert.equal(key(start), key(event), `mismatched terminal event: ${file}:${event.line}`);
    const name = [...stack.map(s => s.name), event.name].join(' > ');
    const id = key({ ...event, path: file, name });
    if (observed.has(id)) error(`duplicate result ${id}`);
    observed.set(id, Object.hasOwn(event, 'skip') ? 'skip' : Object.hasOwn(event, 'todo') ? 'todo' : event.type === 'test:pass' ? 'pass' : 'fail');
    if (file === 'test/conformance/probes/launch.test.js') probeKeys.add(id);
  }
  if ([...stacks.values()].some(s => s.length)) error('unfinished tests in event stream');
} catch (err) { error(`reporter stream: ${err.message}`); }
const statuses = new Map(baseline.map(r => [key(r), r.status]));
const selectedKeys = new Set(selected.map(key));
if (full && (statuses.size !== selected.length || statuses.size !== selectedKeys.size || [...statuses.keys()].some(id => !selectedKeys.has(id)))) error('baseline/classification keys differ');
for (const r of selected) {
  const id = key(r);
  const expected = full ? statuses.get(id) : 'pass';
  if (observed.get(id) !== expected) error(`${id}: expected ${expected}, got ${observed.get(id) ?? 'missing'}`);
  if (observed.get(id) !== 'pass') continue;
  const obligations = sites.filter(s => s.path === r.path && s.case === r.name && s.kind !== 'helper');
  if (r.class !== 'conformance' && !obligations.length) continue;
  if (!credited.has(id)) error(`no attributed launch: ${id}`);
}
for (const [id, status] of observed) {
  if (selectedKeys.has(id)) continue;
  if (probes && probeKeys.has(id)) {
    if (status !== 'pass' || !credited.has(id)) error(`unproven probe: ${id}`);
  } else error(`unexpected registration: ${id}`);
}
if (probes && !probeKeys.size) error('probe file produced no tests');
if (!identity && config.build !== sha256(binary)) error('target binary changed during the run');
if (mode === 'self-test') {
  const alias = path.join(work, 'fixture-alias');
  for (const dir of [process.env.HOME, require('node:os').tmpdir(), plugin]) {
    fs.symlinkSync(dir, alias);
    assert.throws(() => createLog(path.join(alias, 'refused.jsonl'), plugin), /inside a fixture or source directory/);
    fs.unlinkSync(alias);
  }
  let covered = 0;
  for (const s of sites) {
    if (s.kind === 'helper' || s.case === '-' || s.via.startsWith('subtest: ')) continue;
    if (site && !selected.some(r => r.path === s.path && r.name === s.case)) continue;
    const launchSite = `${s.path}:${s.line}`;
    if (!launches.some(l => l.path === s.path && l.name === s.case && l.kind === s.kind && l.witnessPid > 0 &&
        (l.site === launchSite || l.callers?.includes(launchSite)))) {
      error(`no recording binary at ${s.path}:${s.line} for ${s.case}`);
    } else covered++;
  }
  process.stdout.write(`# stand-in coverage: ${covered} inventory rows, ${launches.filter(r => r.witnessPid > 0).length} witnessed launches\n`);
  const binaryConfig = { ...config, target: `binary:${config.witness}`, build: sha256(config.witness),
    log: path.join(work, 'standin.jsonl'), witness: null };
  fs.writeFileSync(preload, `require(${JSON.stringify(path.join(__dirname, 'preload.cjs'))})(${JSON.stringify(binaryConfig)});\n`);
  const check = cp.spawnSync(process.execPath, ['--require', './test/isolate-home.js', '--require', preload,
    '--test', '--test-reporter=tap', path.join(__dirname, 'dispatch.test.js')], { cwd: plugin, encoding: 'utf8' });
  if (check.status !== 0) error(`stand-in dispatch failed:\n${check.stdout}\n${check.stderr}`);
  const records = ndjson(binaryConfig.log);
  if (records.length !== 2 || records.some(r => !validLaunch(r, binaryConfig.target, binaryConfig.build)) ||
      new Set(records.map(r => r.pid)).size !== 2) error('missing independent sync/async binary launches');
  for (const r of records) process.stdout.write(`# stand-in ${JSON.stringify(r)}\n`);
  const metadata = { run_id: 'standin-self-test', commit: '0'.repeat(40) };
  const exported = cp.execFileSync(process.execPath, [path.join(root, 'hack/conformance-launches'), binaryConfig.log, metadata.run_id, metadata.commit], { encoding: 'utf8' });
  assert.deepEqual(exported.trimEnd().split('\n'), [JSON.stringify(metadata), 'path\tline\tcolumn\tname\tbinary\tbinary_sha256',
    ...records.map(r => [r.path, r.line, r.column, r.name, config.witness, binaryConfig.build].join('\t'))]);
  process.stdout.write(`# stand-in TSV: ${records.length} measured launches, matching metadata and rows\n`);
}
const counts = Object.fromEntries(['pass', 'fail', 'skip', 'todo'].map(s => [s, [...observed.values()].filter(v => v === s).length]));
process.stdout.write(`# conformance ${JSON.stringify({ target, mode, selected: selected.length, counts, launches: launches.length, ok: !failed })}\n`);
process.exitCode = failed ? 1 : 0;
