// Copyright 2026 The claude-code-model-gateway Authors.
// SPDX-License-Identifier: Apache-2.0
'use strict';

const assert = require('node:assert/strict');
const cp = require('node:child_process');
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const [root, plugin, work, target, mode, phase, pkg, gate, requestedLog, classification, site, capture, candidateFile] = process.argv.slice(2);
const { PROBE_FILE, REQUIRED_PROBES, checkPinnedLaunchForms, createLog, evaluateSuite, parentCredits, tsv, validLaunch } = require('./evidence.cjs');
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
const expectedFiles = [...files];
if (probes) {
  files.push(path.join(data, 'probes/launch.test.js'));
  expectedFiles.push(PROBE_FILE);
  pattern.push(...REQUIRED_PROBES.map(p => escapeRE(p.name)));
}
// Verify the pinned test tree uses only inventoried launch forms and matches the reviewed pin.
const sourceDigest = checkPinnedLaunchForms(plugin);
const manifest = require(path.join(plugin, 'package.json')).scripts.test;
assert.equal(manifest, 'node --require ./test/isolate-home.js --test --test-concurrency=2 --test-timeout=300000 "test/*.test.js"', 'unexpected pinned test command');
const eventsFile = path.join(work, 'events.jsonl');
const result = cp.spawnSync(process.execPath, ['--require', './test/isolate-home.js', '--require', preload,
  '--test', '--test-concurrency=2', '--test-timeout=300000', '--test-reporter=tap', '--test-reporter-destination=stdout',
  `--test-reporter=${path.join(root, 'hack/baseline-node-reporter.mjs')}`, `--test-reporter-destination=${eventsFile}`,
  ...full ? [] : [`--test-name-pattern=^(?:${pattern.join('|')})$`], ...files], { cwd: plugin, stdio: 'inherit' });
const launches = ndjson(log);
for (const record of launches) process.stdout.write(`# launch ${JSON.stringify(record)}\n`);
const sites = inventory('launch-sites.tsv');
let events = [];
let eventsError = null;
try {
  events = ndjson(eventsFile);
} catch (err) {
  eventsError = `reporter stream: ${err.message}`;
}
const normalizeFile = f => f.startsWith('..') ? path.relative(root, path.resolve(plugin, f)) : f;
const verdict = evaluateSuite({
  target,
  build: config.build,
  full,
  probes,
  selected,
  baseline,
  sites,
  launches,
  events,
  expectedFiles,
  runnerStatus: result.status,
  runnerError: result.error?.message ?? eventsError,
  normalizeFile,
  binaryUnchanged: identity || config.build === sha256(binary),
});
let failed = verdict.failed;
function error(message) { failed = true; process.stderr.write(`conformance: ${message}\n`); }
for (const message of verdict.errors) process.stderr.write(`conformance: ${message}\n`);

const tsvLine = values => values.map(v => String(v).replace(/[\\\t\r\n]/g,
  c => ({ '\\': '\\\\', '\t': '\\t', '\r': '\\r', '\n': '\\n' })[c])).join('\t') + '\n';
function launchTable(records, image, digest) {
  const parent = parentCredits(sites, baseline);
  let text = 'path\tline\tcolumn\tname\tbinary\tbinary_sha256\n';
  for (const record of records) {
    assert(validLaunch(record, image, digest), 'invalid or mixed launch evidence');
    for (const r of [record, parent(record)].filter(Boolean)) {
      text += tsvLine([r.path, r.line, r.column, r.name, record.target.slice(7), record.build]);
    }
  }
  return text;
}
if (capture && candidateFile && capture === binary && !failed && mode === 'suite' && !identity && !(phase || pkg || gate || site)) {
  const safeCandidate = createLog(candidateFile, plugin);
  let census = 'path\tline\tcolumn\tdepth\tname\tstatus\tdetail\n';
  for (const [id, status] of verdict.observed) {
    const [file, line, column, name] = JSON.parse(id);
    census += tsvLine([file, line, column, verdict.depths.get(id), name, status, '-']);
  }
  // These are private observations, not evidence. Only the producer has the final verdict.
  fs.writeFileSync(safeCandidate, JSON.stringify({
    binary, binary_sha256: config.build, test_source_sha256: sourceDigest, census,
    launches: launchTable(launches, target, config.build),
  }), { mode: 0o600 });
}

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
  const standinBuild = sha256(config.witness);
  const binaryConfig = { ...config, target: `binary:${config.witness}`, build: standinBuild,
    log: path.join(work, 'standin.jsonl'), witness: null, trackActive: false };
  fs.writeFileSync(preload, `require(${JSON.stringify(path.join(__dirname, 'preload.cjs'))})(${JSON.stringify(binaryConfig)});\n`);
  const check = cp.spawnSync(process.execPath, ['--require', './test/isolate-home.js', '--require', preload,
    '--test', '--test-reporter=tap', '--test-reporter-destination=stdout',
    `--test-reporter=${path.join(root, 'hack/baseline-node-reporter.mjs')}`,
    `--test-reporter-destination=${path.join(work, 'standin-events.jsonl')}`, path.join(__dirname, 'dispatch.test.js')], { cwd: plugin, encoding: 'utf8' });
  if (check.status !== 0) error(`stand-in dispatch failed:\n${check.stdout}\n${check.stderr}`);
  const records = ndjson(binaryConfig.log);
  if (records.length !== 2 || records.some(r => !validLaunch(r, binaryConfig.target, binaryConfig.build)) ||
      new Set(records.map(r => r.pid)).size !== 2) error('missing independent sync/async binary launches');
  for (const r of records) process.stdout.write(`# stand-in ${JSON.stringify(r)}\n`);
  const standinTable = launchTable(records, binaryConfig.target, binaryConfig.build);
  fs.writeFileSync(path.join(work, 'standin-table'), standinTable);
  assert.deepEqual(standinTable.trimEnd().split('\n'), ['path\tline\tcolumn\tname\tbinary\tbinary_sha256',
    ...records.map(r => [r.path, r.line, r.column, r.name, config.witness, binaryConfig.build].join('\t'))]);
  process.stdout.write(`# stand-in TSV: ${records.length} measured launches; dispatch fixture only\n`);

  if (full) {
    const key = require('./evidence.cjs').key;
    const counts = new Map(classes.filter(r => r.class === 'conformance').map(r => [key(r), 0]));
    const parent = parentCredits(sites, baseline);
    for (const launch of launches) {
      for (const r of [launch, parent(launch)].filter(Boolean)) {
        const id = key(r);
        if (counts.has(id)) counts.set(id, counts.get(id) + 1);
      }
    }
    const cases = [], credits = [];
    for (const [id, n] of counts) {
      const [f, l, c, name] = JSON.parse(id);
      cases.push(tsvLine([f, l, c, verdict.depths.get(id), name]));
      credits.push(tsvLine([f, l, c, name, n]));
    }
    const fingerprint = rows => crypto.createHash('sha256').update(rows.sort().join('')).digest('hex');
    fs.writeFileSync(path.join(work, 'identity-profile.json'), JSON.stringify([fingerprint(cases), fingerprint(credits)]));
  }
  const requiredControls = [
    'control: unattributed, alternate-node, and extra-option product launches fail closed while decoys stay on Node',
    'control: real product launch falling back to Node inside a passing case fails the run',
    'control: passing two-launch case fails when only one inventoried launch is redirected',
    'control: withholding one execution of a passing repeated site fails attribution',
    'control: static launch-form check rejects out-of-scope forms in pinned sources',
    'control: premature suite summary before remaining events fails',
    'control: withholding one required probe fails run.cjs',
  ];
  const controlEventsFile = path.join(work, 'control-events.jsonl');
  const controls = cp.spawnSync(process.execPath, [
    '--test',
    '--test-reporter=tap', '--test-reporter-destination=stdout',
    `--test-reporter=${path.join(root, 'hack/baseline-node-reporter.mjs')}`,
    `--test-reporter-destination=${controlEventsFile}`,
    path.join(__dirname, 'controls.test.js'),
  ], {
    cwd: plugin,
    encoding: 'utf8',
    env: {
      ...process.env,
      ADAPTER_CTRL_ROOT: root,
      ADAPTER_CTRL_PLUGIN: plugin,
      ADAPTER_CTRL_WORK: work,
      ADAPTER_CTRL_STANDIN: config.witness,
      ADAPTER_CTRL_BUILD: standinBuild,
    },
  });
  let controlEvents = [];
  let controlError = null;
  try {
    controlEvents = ndjson(controlEventsFile);
  } catch (err) {
    controlError = `control reporter stream: ${err.message}`;
  }
  const passedControls = new Set();
  const failedControls = [];
  let controlSummary = null;
  for (const ev of controlEvents) {
    if (ev.type === 'test:pass' && ev.nesting === 0 && !Object.hasOwn(ev, 'skip') && !Object.hasOwn(ev, 'todo')) {
      passedControls.add(ev.name);
    } else if (ev.type === 'test:fail' && ev.nesting === 0) {
      failedControls.push(ev.name);
    } else if (ev.type === 'test:summary' && ev.file == null) {
      controlSummary = ev;
    }
  }
  const missingControls = requiredControls.filter(name => !passedControls.has(name));
  const extraControls = [...passedControls].filter(name => !requiredControls.includes(name));
  if (controls.status !== 0 || controlError || !controlSummary?.success || failedControls.length ||
      missingControls.length || extraControls.length || controlSummary.counts?.passed !== requiredControls.length) {
    error(`negative controls failed (status=${controls.status}, missing=${JSON.stringify(missingControls)}, extra=${JSON.stringify(extraControls)}, failed=${JSON.stringify(failedControls)}, err=${controlError}):\n${controls.stdout}\n${controls.stderr}`);
  } else {
    process.stdout.write(`# stand-in negative controls: ${requiredControls.length} controls verified (${requiredControls.join('; ')})\n`);
  }
}
process.stdout.write(`# conformance ${JSON.stringify({ target, mode, selected: selected.length, counts: verdict.counts, launches: launches.length, ok: !failed })}\n`);
process.exitCode = failed ? 1 : 0;
