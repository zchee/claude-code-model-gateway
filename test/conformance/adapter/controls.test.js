// Copyright 2026 The claude-code-model-gateway Authors.
// SPDX-License-Identifier: Apache-2.0
'use strict';

const assert = require('node:assert/strict');
const cp = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const { evaluateSuite, key, tsv } = require('./evidence.cjs');

const root = process.env.ADAPTER_CTRL_ROOT;
const plugin = process.env.ADAPTER_CTRL_PLUGIN;
const work = process.env.ADAPTER_CTRL_WORK;
const standin = process.env.ADAPTER_CTRL_STANDIN;
const build = process.env.ADAPTER_CTRL_BUILD;
assert(root && plugin && work && standin && build, 'missing adapter control environment');

const data = path.join(root, 'test/conformance');
const baseline = tsv(path.join(data, 'baseline/census.tsv'));
const classes = tsv(path.join(data, 'classification.tsv'));
const sites = tsv(path.join(data, 'launch-sites.tsv'));
const ndjson = file => fs.readFileSync(file, 'utf8').split('\n').filter(Boolean).map(line => JSON.parse(line));
function cleanEnv() {
  const env = { ...process.env };
  delete env.NODE_TEST_CONTEXT;
  return env;
}

test('control: unattributed, alternate-node, and extra-option product launches fail closed while decoys stay on Node', () => {
  const dir = fs.mkdtempSync(path.join(work, 'ctrl-mixed-'));
  const log = path.join(dir, 'launches.jsonl');
  fs.writeFileSync(log, '');
  const preload = path.join(dir, 'preload.cjs');
  const cfg = { root, plugin, target: `binary:${standin}`, work: dir, log, build, witness: null, trackActive: false };
  fs.writeFileSync(preload, `require(${JSON.stringify(path.join(__dirname, 'preload.cjs'))})(${JSON.stringify(cfg)});\n`);

  const run = cp.spawnSync(process.execPath, [
    '--require', './test/isolate-home.js',
    '--require', preload,
    '--test', '--test-reporter=tap',
    path.join(__dirname, 'mixed-target.test.js'),
  ], { cwd: plugin, encoding: 'utf8', env: cleanEnv() });
  assert.equal(run.status, 0, `${run.stdout}\n${run.stderr}`);

  const records = ndjson(log);
  const valid = records.filter(r => !r.unattributed);
  const rejected = records.filter(r => r.unattributed);
  assert.equal(valid.length, 1, `expected 1 redirected binary launch, got ${valid.length}`);
  assert.equal(valid[0].target, `binary:${standin}`);
  assert.equal(valid[0].build, build);
  assert.equal(rejected.length, 8, `expected 8 fail-closed unattributed records, got ${rejected.length}`);
  assert.ok(rejected.some(r => /outside inventoried sites/.test(r.reason)));
  assert.ok(rejected.some(r => /unsupported Node options before product entry/.test(r.reason)));
  assert.ok(rejected.some(r => /child_process\.execFileSync/.test(r.reason)));

  // Even if the test caught every throw and exited 0, the suite evaluator must reject the run.
  const syntheticEvent = { file: 'test/conformance/adapter/mixed-target.test.js', line: 1, column: 1, nesting: 0, name: 'mixed' };
  const verdict = evaluateSuite({
    target: `binary:${standin}`,
    build,
    full: false,
    probes: false,
    selected: [{ path: syntheticEvent.file, line: '1', column: '1', name: 'mixed', class: 'conformance' }],
    baseline,
    sites,
    launches: records,
    events: [
      { type: 'test:start', ...syntheticEvent },
      { type: 'test:pass', ...syntheticEvent },
      { type: 'test:summary', file: syntheticEvent.file, success: true, counts: { tests: 1, passed: 1, failed: 0, skipped: 0, todo: 0, cancelled: 0 } },
      { type: 'test:summary', file: null, success: true, counts: { tests: 1, passed: 1, failed: 0, skipped: 0, todo: 0, cancelled: 0 } },
    ],
    expectedFiles: [syntheticEvent.file],
    runnerStatus: 0,
  });
  assert.equal(verdict.failed, true);
  assert.ok(verdict.errors.some(e => e.includes('unattributed or unredirected product launch')),
    `expected unattributed error, got: ${verdict.errors.join('; ')}`);
});

test('control: real product launch falling back to Node inside a passing case fails the run', () => {
  // Part A: repeated-site mixed-target control on the real three-iteration case
  // test/wiring-mode.test.js:137 (2 binary launches + 1 Node fallback at the same row).
  const caseName = 'retired wiring-mode flags fail loudly';
  const row = classes.find(r => r.path === 'test/wiring-mode.test.js' && r.name === caseName);
  assert(row, 'expected wiring-mode case in classification');
  const obligations = sites.filter(s => s.path === row.path && s.case === row.name && s.kind !== 'helper');
  assert.equal(obligations.length, 1, 'expected one inventoried row executed repeatedly');
  const siteCoord = `${row.path}:${obligations[0].line}`;

  const events = [
    { type: 'test:start', file: row.path, line: Number(row.line), column: Number(row.column), nesting: 0, name: row.name },
    { type: 'test:pass', file: row.path, line: Number(row.line), column: Number(row.column), nesting: 0, name: row.name },
    { type: 'test:summary', file: row.path, success: true, counts: { tests: 1, passed: 1, failed: 0, skipped: 0, todo: 0, cancelled: 0 } },
    { type: 'test:summary', file: null, success: true, counts: { tests: 1, passed: 1, failed: 0, skipped: 0, todo: 0, cancelled: 0 } },
  ];
  const baseRec = { path: row.path, line: Number(row.line), column: Number(row.column), name: row.name,
    site: siteCoord, kind: obligations[0].kind };
  const twoBinOneNode = [
    { ...baseRec, target: `binary:${standin}`, pid: 2001, build },
    { ...baseRec, target: `binary:${standin}`, pid: 2002, build },
    { ...baseRec, target: 'identity', pid: 2003, build: null },
  ];
  const mixedVerdict = evaluateSuite({
    target: `binary:${standin}`, build, full: false, probes: false,
    selected: [row], baseline, sites, launches: twoBinOneNode, events, expectedFiles: [row.path], runnerStatus: 0,
  });
  assert.equal(mixedVerdict.failed, true);
  assert.ok(mixedVerdict.errors.some(e => e.includes(`invalid launch at ${siteCoord}`)),
    `expected invalid launch error on repeated site, got: ${mixedVerdict.errors.join('; ')}`);

  const threeBin = [
    { ...baseRec, target: `binary:${standin}`, pid: 2001, build },
    { ...baseRec, target: `binary:${standin}`, pid: 2002, build },
    { ...baseRec, target: `binary:${standin}`, pid: 2003, build },
  ];
  const allBinVerdict = evaluateSuite({
    target: `binary:${standin}`, build, full: false, probes: false,
    selected: [row], baseline, sites, launches: threeBin, events, expectedFiles: [row.path], runnerStatus: 0,
  });
  assert.equal(allBinVerdict.failed, false, allBinVerdict.errors.join('; '));

  // Part B: live passing test executes one redirected launch and one real product launch
  // attempting Node fallback (caught so the test itself exits 0); the suite verdict fails.
  const dir = fs.mkdtempSync(path.join(work, 'ctrl-fallback-'));
  const log = path.join(dir, 'launches.jsonl');
  fs.writeFileSync(log, '');
  const preload = path.join(dir, 'preload.cjs');
  const cfg = { root, plugin, target: `binary:${standin}`, work: dir, log, build, witness: null, trackActive: false };
  fs.writeFileSync(preload, `require(${JSON.stringify(path.join(__dirname, 'preload.cjs'))})(${JSON.stringify(cfg)});\n`);
  const caseFile = path.join(__dirname, 'fallback-pass.test.js');
  const eventsPath = path.join(dir, 'events.jsonl');
  const live = cp.spawnSync(process.execPath, [
    '--require', './test/isolate-home.js',
    '--require', preload,
    '--test',
    '--test-reporter=tap', '--test-reporter-destination=stdout',
    `--test-reporter=${path.join(root, 'hack/baseline-node-reporter.mjs')}`,
    `--test-reporter-destination=${eventsPath}`,
    caseFile,
  ], { cwd: plugin, encoding: 'utf8', env: cleanEnv() });
  assert.equal(live.status, 0, `expected inner test to pass on its own:\n${live.stdout}\n${live.stderr}`);
  const liveEvents = ndjson(eventsPath);
  const liveLaunches = ndjson(log);
  const passEv = liveEvents.find(e => e.type === 'test:pass');
  assert(passEv, 'expected inner passing test event');
  const norm = f => path.relative(root, path.resolve(plugin, f));
  const relFile = norm(passEv.file);
  const liveVerdict = evaluateSuite({
    target: `binary:${standin}`, build, full: false, probes: false,
    selected: [{ path: relFile, line: String(passEv.line), column: String(passEv.column), name: passEv.name, class: 'conformance' }],
    baseline, sites, launches: liveLaunches, events: liveEvents, expectedFiles: [relFile], runnerStatus: live.status, normalizeFile: norm,
  });
  assert.equal(liveVerdict.failed, true);
  assert.ok(liveVerdict.errors.some(e => e.includes('unattributed or unredirected product launch')),
    `expected unattributed error from live Node-fallback attempt, got: ${liveVerdict.errors.join('; ')}`);
});

test('control: passing two-launch case fails when only one inventoried launch is redirected', () => {
  const caseName = 'a second supervisor exits when the singleton listener is already owned';
  const row = classes.find(r => r.path === 'test/gateway-drain.test.js' && r.name === caseName);
  assert(row, 'expected singleton supervisor case in classification');
  const obligations = sites.filter(s => s.path === row.path && s.case === row.name && s.kind !== 'helper');
  assert.equal(obligations.length, 2, 'expected two supervisor obligations for singleton case');

  const events = [
    { type: 'test:start', file: row.path, line: Number(row.line), column: Number(row.column), nesting: 0, name: row.name },
    { type: 'test:pass', file: row.path, line: Number(row.line), column: Number(row.column), nesting: 0, name: row.name },
    { type: 'test:summary', file: row.path, success: true, counts: { tests: 1, passed: 1, failed: 0, skipped: 0, todo: 0, cancelled: 0 } },
    { type: 'test:summary', file: null, success: true, counts: { tests: 1, passed: 1, failed: 0, skipped: 0, todo: 0, cancelled: 0 } },
  ];
  const launch1 = { path: row.path, line: Number(row.line), column: Number(row.column), name: row.name,
    site: `${row.path}:${obligations[0].line}`, kind: obligations[0].kind, target: `binary:${standin}`, pid: 1001, build };
  const launch2 = { path: row.path, line: Number(row.line), column: Number(row.column), name: row.name,
    site: `${row.path}:${obligations[1].line}`, kind: obligations[1].kind, target: `binary:${standin}`, pid: 1002, build };

  const partial = evaluateSuite({
    target: `binary:${standin}`, build, full: false, probes: false,
    selected: [row], baseline, sites, launches: [launch1], events, expectedFiles: [row.path], runnerStatus: 0,
  });
  assert.equal(partial.failed, true);
  assert.ok(partial.errors.some(e => e.includes(`missing attributed launch at ${row.path}:${obligations[1].line}`)),
    `expected missing second launch error, got: ${partial.errors.join('; ')}`);

  const complete = evaluateSuite({
    target: `binary:${standin}`, build, full: false, probes: false,
    selected: [row], baseline, sites, launches: [launch1, launch2], events, expectedFiles: [row.path], runnerStatus: 0,
  });
  assert.equal(complete.failed, false, complete.errors.join('; '));
  assert.deepEqual(complete.errors, []);

  // Also prove truncated streams missing terminal summaries fail even when all launches pass.
  const truncated = evaluateSuite({
    target: `binary:${standin}`, build, full: false, probes: false,
    selected: [row], baseline, sites, launches: [launch1, launch2],
    events: events.slice(0, 2), expectedFiles: [row.path], runnerStatus: 0,
  });
  assert.equal(truncated.failed, true);
  assert.ok(truncated.errors.some(e => e.includes(`missing test summary: ${row.path}`)),
    `expected missing file summary error, got: ${truncated.errors.join('; ')}`);
  assert.ok(truncated.errors.some(e => e.includes('missing test summary: suite')),
    `expected missing suite summary error, got: ${truncated.errors.join('; ')}`);
});

test('control: premature suite summary before remaining events fails', () => {
  const caseName = 'a second supervisor exits when the singleton listener is already owned';
  const row = classes.find(r => r.path === 'test/gateway-drain.test.js' && r.name === caseName);
  const obligations = sites.filter(s => s.path === row.path && s.case === row.name && s.kind !== 'helper');
  const launch1 = { path: row.path, line: Number(row.line), column: Number(row.column), name: row.name,
    site: `${row.path}:${obligations[0].line}`, kind: obligations[0].kind, target: `binary:${standin}`, pid: 1001, build };
  const launch2 = { path: row.path, line: Number(row.line), column: Number(row.column), name: row.name,
    site: `${row.path}:${obligations[1].line}`, kind: obligations[1].kind, target: `binary:${standin}`, pid: 1002, build };
  const reordered = [
    { type: 'test:start', file: row.path, line: Number(row.line), column: Number(row.column), nesting: 0, name: row.name },
    { type: 'test:pass', file: row.path, line: Number(row.line), column: Number(row.column), nesting: 0, name: row.name },
    { type: 'test:summary', file: null, success: true, counts: { tests: 1, passed: 1, failed: 0, skipped: 0, todo: 0, cancelled: 0 } },
    { type: 'test:summary', file: row.path, success: true, counts: { tests: 1, passed: 1, failed: 0, skipped: 0, todo: 0, cancelled: 0 } },
  ];
  const verdict = evaluateSuite({
    target: `binary:${standin}`, build, full: false, probes: false,
    selected: [row], baseline, sites, launches: [launch1, launch2],
    events: reordered, expectedFiles: [row.path], runnerStatus: 0,
  });
  assert.equal(verdict.failed, true);
  assert.ok(verdict.errors.some(e => e.includes('event after suite summary')),
    `expected event-after-suite-summary error, got: ${verdict.errors.join('; ')}`);
});

test('control: withholding one required probe fails run.cjs', () => {
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(work, 'ctrl-probe-')));
  const fakeRoot = path.join(dir, 'root');
  const fakeWork = path.join(dir, 'work');
  fs.mkdirSync(path.join(fakeRoot, 'test/conformance/probes'), { recursive: true });
  fs.mkdirSync(fakeWork, { recursive: true });
  fs.symlinkSync(path.join(root, 'hack'), path.join(fakeRoot, 'hack'));
  for (const name of ['adapter', 'baseline', 'classification.tsv', 'launch-sites.tsv']) {
    fs.symlinkSync(path.join(data, name), path.join(fakeRoot, 'test/conformance', name));
  }
  const source = fs.readFileSync(path.join(data, 'probes/launch.test.js'), 'utf8');
  const withheldName = 'launch probe: standalone worker exits when fixture IPC disconnects';
  const stripped = source.replace(
    /test\('launch probe: standalone worker exits when fixture IPC disconnects',[\s\S]*?\n\}\);\n/,
    '',
  );
  assert.notEqual(stripped, source, 'failed to strip target probe from copy');
  fs.writeFileSync(path.join(fakeRoot, 'test/conformance/probes/launch.test.js'), stripped);

  const run = cp.spawnSync(process.execPath, [
    path.join(__dirname, 'run.cjs'),
    fakeRoot, plugin, fakeWork,
    'identity', 'probes', '', '', '', path.join(fakeWork, 'launches.jsonl'), '', '',
  ], { encoding: 'utf8', env: cleanEnv() });
  assert.equal(run.status, 1, `expected failure when a probe is withheld:\n${run.stdout}\n${run.stderr}`);
  assert.match(run.stderr, new RegExp(`missing required probe: ${withheldName}`));
  const summaryMatch = run.stdout.match(/^# conformance (\{.*\})$/m);
  assert(summaryMatch, `missing conformance summary in stdout:\n${run.stdout}`);
  const summary = JSON.parse(summaryMatch[1]);
  assert.equal(summary.ok, false);
  assert.equal(summary.counts.pass, 4, `expected 3 remaining probes + 1 pinned case to pass, got ${JSON.stringify(summary.counts)}`);
  assert.equal(summary.counts.fail, 0);
  assert.equal(key({ path: 'a', line: 1, column: 1, name: 'b' }), '["a",1,1,"b"]');
});
