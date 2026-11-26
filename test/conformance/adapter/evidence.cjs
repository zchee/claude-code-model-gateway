// Copyright 2026 The claude-code-model-gateway Authors.
// SPDX-License-Identifier: Apache-2.0
'use strict';

const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const PINNED_TEST_DIGEST = '4a10b3bb214b5ecc779c18fa1ba16ff0943168ec00ed881d358449a147b0ca06';
const ALLOWED_PROXY_NODE_COPIES = new Set([
  'codex-upstream-cases.test.js:240',
  'discovery-cache.test.js:75',
  'discovery-cache.test.js:77',
  'gateway-process-isolation.test.js:201',
  'gateway-process-isolation.test.js:1189',
  'recovery-lifecycle.test.js:66',
  'recovery-lifecycle.test.js:96',
  'recovery-lifecycle.test.js:132',
]);

function checkPinnedLaunchForms(plugin) {
  const testRoot = path.join(plugin, 'test');
  const sourceDigest = crypto.createHash('sha256');
  const violations = [];
  const entries = fs.readdirSync(testRoot, { recursive: true }).map(n => n.replaceAll('\\', '/')).sort();
  for (const name of entries) {
    const file = path.join(testRoot, name);
    const stat = fs.lstatSync(file);
    if (stat.isDirectory()) continue;
    assert(stat.isFile(), `unexpected pinned test entry: ${name}`);
    const bytes = fs.readFileSync(file);
    sourceDigest.update(`${name}\0`).update(crypto.createHash('sha256').update(bytes).digest());
    if (!/\.(?:c|m)?js$/.test(name)) continue;
    const lines = bytes.toString('utf8').split('\n');
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i];
      const loc = `${name}:${i + 1}`;
      if (/\bworker_threads\b/.test(line)) violations.push(`${loc}: worker_threads`);
      if (/\bnode:cluster\b|require\(['"]cluster['"]\)/.test(line)) violations.push(`${loc}: cluster`);
      if (/\bshell\s*:/.test(line)) violations.push(`${loc}: shell option`);
      if (/\b(?:execSync|execFile|execFileSync|fork)\s*\(/.test(line) ||
          (/(?:^|[^/])\bexec\s*\(/.test(line) && !/\.exec\(/.test(line))) {
        violations.push(`${loc}: uninventoried child_process API`);
      }
      if (/(?:copyFileSync|linkSync|symlinkSync)\s*\([^)]*process\.execPath/.test(line) &&
          !ALLOWED_PROXY_NODE_COPIES.has(loc)) {
        violations.push(`${loc}: unreviewed copy of process.execPath`);
      }
    }
  }
  assert.equal(violations.length, 0,
    `out-of-scope launch forms in pinned test sources: ${violations.join('; ')}`);
  assert.equal(sourceDigest.digest('hex'), PINNED_TEST_DIGEST,
    'pinned test sources changed: review launch forms and execution counts');
}

const PROBE_FILE = 'test/conformance/probes/launch.test.js';
const REQUIRED_PROBES = [
  { name: 'launch probe: stdout listener, health pid and fixture IPC shutdown', kind: 'supervisor' },
  { name: 'launch probe: standalone worker accepts fixture drain', kind: 'worker' },
  { name: 'launch probe: standalone worker exits when fixture IPC disconnects', kind: 'worker' },
  { name: 'launch probe: copied proxy is real Node and fixture NODE_OPTIONS survives', kind: 'worker' },
];

function tsv(file) {
  const [header, ...lines] = fs.readFileSync(file, 'utf8').trimEnd().split('\n');
  const columns = header.split('\t');
  return lines.map(line => Object.fromEntries(line.split('\t').map((value, i) =>
    [columns[i], value.replace(/\\([\\tnr])/g, (_, c) => ({ '\\': '\\', t: '\t', n: '\n', r: '\r' })[c])])));
}

function createLog(request, plugin) {
  assert(path.isAbsolute(request) && path.normalize(request) === request && !request.endsWith('/'),
    'LAUNCH_LOG must be an absolute clean file path');
  const log = path.join(fs.realpathSync(path.dirname(request)), path.basename(request));
  for (const dir of [process.env.HOME, os.tmpdir(), plugin].map(p => fs.realpathSync(p))) {
    const relative = path.relative(dir, log);
    assert(relative === '..' || relative.startsWith(`..${path.sep}`), 'LAUNCH_LOG is inside a fixture or source directory');
  }
  fs.closeSync(fs.openSync(log, 'wx', 0o600));
  return log;
}

const key = r => JSON.stringify([r.path, Number(r.line), Number(r.column), r.name]);
const validLaunch = (r, target, build) =>
  !r.unattributed && r.target === target && r.build === build && Number.isSafeInteger(r.pid) && r.pid > 0;

// Only an inventory edge to this exact nested registration may also credit its parent.
function parentCredits(sites, census) {
  const result = new Map();
  for (const site of sites.filter(s => s.via.startsWith('subtest: '))) {
    const byName = name => census.filter(r => r.path === site.path && r.name === name);
    const parents = byName(site.case), children = byName(`${site.case} > ${site.via.slice(9)}`);
    assert(parents.length === 1 && children.length === 1, 'ambiguous nested launch registration');
    result.set(JSON.stringify([key(children[0]), `${site.path}:${site.line}`, site.kind]), parents[0]);
  }
  return r => result.get(JSON.stringify([key(r), r.site, r.kind]));
}

function evaluateSuite({
  target, build, full, probes, selected, baseline, sites, launches, events,
  expectedFiles, runnerStatus, runnerError = null, normalizeFile = f => f, binaryUnchanged = true,
}) {
  const errors = [];
  let failed = runnerStatus !== 0;
  const error = message => { failed = true; errors.push(message); };
  if (runnerError) error(runnerError);

  const repeated = new Map(tsv(path.join(__dirname, 'repeated-sites.tsv')).map(r => [r.site, Number(r.count)]));
  const parent = parentCredits(sites, baseline);
  const validLaunches = [];
  const credited = new Set();
  for (const r of launches) {
    if (r.unattributed) {
      error(`unattributed or unredirected product launch: ${r.reason ?? 'unknown'}`);
      continue;
    }
    if (!validLaunch(r, target, build)) {
      error(`invalid launch at ${r.site}`);
      continue;
    }
    validLaunches.push(r);
    credited.add(key(r));
    if (parent(r)) credited.add(key(parent(r)));
  }

  const observed = new Map();
  const observedProbes = new Map();
  const probeIds = new Set();
  const observedByFile = new Map();
  const ends = new Map();
  const stacks = new Map();
  const expectedFileSet = new Set(expectedFiles);
  const bump = (file, status) => {
    if (!observedByFile.has(file)) observedByFile.set(file, { pass: 0, fail: 0, skip: 0, todo: 0 });
    observedByFile.get(file)[status]++;
  };

  try {
    for (const event of events) {
      if (event.type === 'baseline:environment') continue;
      if (['test:abort', 'test:aborted', 'test:cancelled'].includes(event.type) ||
          ['cancelledByParent', 'testAborted', 'testTimeoutFailure'].includes(event.failureType)) {
        error(`aborted test: ${event.name}`);
      }
      const file = event.file == null ? null : normalizeFile(event.file);
      if (ends.has(null)) {
        error(`event after suite summary: ${event.type}${file ? ` (${file})` : ''}`);
        continue;
      }
      if (event.type === 'test:summary') {
        if (!event.counts || event.counts.cancelled || !event.success) {
          error(`unsuccessful test summary: ${file ?? 'suite'}`);
        }
        if (file !== null && !expectedFileSet.has(file)) {
          error(`unknown test summary: ${file}`);
        } else if (ends.has(file)) {
          error(`duplicate test summary: ${file ?? 'suite'}`);
        } else {
          ends.set(file, event);
        }
        continue;
      }
      if (!['test:start', 'test:pass', 'test:fail'].includes(event.type)) continue;
      if (file === null || !expectedFileSet.has(file)) {
        error(`unknown event file: ${file}`);
        continue;
      }
      if (ends.has(file)) error(`event after file summary: ${file}`);
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
      const status = Object.hasOwn(event, 'skip') ? 'skip' : Object.hasOwn(event, 'todo') ? 'todo' :
        event.type === 'test:pass' ? 'pass' : 'fail';
      observed.set(id, status);
      bump(file, status);
      if (file === PROBE_FILE) {
        probeIds.add(id);
        if (observedProbes.has(name)) error(`duplicate probe name: ${name}`);
        observedProbes.set(name, { id, status });
      }
    }
    if ([...stacks.values()].some(s => s.length)) error('unfinished tests in event stream');
  } catch (err) {
    error(`reporter stream: ${err.message}`);
  }

  for (const expectedFile of [...expectedFileSet, null]) {
    if (!ends.has(expectedFile)) error(`missing test summary: ${expectedFile ?? 'suite'}`);
  }
  const totals = { pass: 0, fail: 0, skip: 0, todo: 0 };
  for (const c of observedByFile.values()) {
    totals.pass += c.pass;
    totals.fail += c.fail;
    totals.skip += c.skip;
    totals.todo += c.todo;
  }
  for (const [file, summary] of ends) {
    const c = file === null ? totals : (observedByFile.get(file) ?? { pass: 0, fail: 0, skip: 0, todo: 0 });
    const total = c.pass + c.fail + c.skip + c.todo;
    const sc = summary.counts;
    if (!sc || sc.tests !== total || sc.passed !== c.pass || sc.failed !== c.fail ||
        sc.skipped !== c.skip || sc.todo !== c.todo || sc.cancelled !== 0) {
      error(`summary count mismatch for ${file ?? 'suite'}: runner=${JSON.stringify(sc)} observed=${JSON.stringify({ tests: total, ...c })}`);
    }
    if (Boolean(summary.success) !== (c.fail === 0)) {
      error(`summary success mismatch for ${file ?? 'suite'}`);
    }
  }
  if (ends.has(null) && (runnerStatus === 0) !== Boolean(ends.get(null).success)) {
    error('suite exit status disagrees with runner summary');
  }

  const statuses = new Map(baseline.map(r => [key(r), r.status]));
  const selectedKeys = new Set(selected.map(key));
  if (full && (statuses.size !== selected.length || statuses.size !== selectedKeys.size ||
      [...statuses.keys()].some(id => !selectedKeys.has(id)))) {
    error('baseline/classification keys differ');
  }

  for (const r of selected) {
    const id = key(r);
    const expected = full ? statuses.get(id) : 'pass';
    if (observed.get(id) !== expected) error(`${id}: expected ${expected}, got ${observed.get(id) ?? 'missing'}`);
    if (observed.get(id) !== 'pass') continue;
    const obligations = sites.filter(s => s.path === r.path && s.case === r.name && s.kind !== 'helper');
    if (r.class !== 'conformance' && !obligations.length) continue;
    if (!obligations.length) {
      if (!credited.has(id)) error(`no attributed launch: ${id}`);
      continue;
    }
    for (const s of obligations) {
      const launchSite = `${s.path}:${s.line}`;
      const nested = s.via.startsWith('subtest: ');
      const matches = validLaunches.filter(l => l.kind === s.kind &&
        (l.site === launchSite || l.callers?.includes(launchSite)) && (nested ?
          l.path === s.path && l.name === `${s.case} > ${s.via.slice(9)}` && parent(l) && key(parent(l)) === id : key(l) === id));
      const expectedCount = repeated.get(launchSite) ?? 1;
      const count = new Set(matches.map(l => l.pid)).size;
      if (count !== expectedCount || matches.length !== expectedCount) {
        error(`${count ? 'wrong number of' : 'missing'} attributed launch at ${launchSite} (${s.kind}) for ${id}: expected ${expectedCount}, got ${count} distinct PIDs in ${matches.length} records`);
      }
    }
  }

  if (probes) {
    const requiredNames = new Set(REQUIRED_PROBES.map(p => p.name));
    for (const { name, kind } of REQUIRED_PROBES) {
      const probe = observedProbes.get(name);
      if (!probe) {
        error(`missing required probe: ${name}`);
      } else if (probe.status !== 'pass' || !validLaunches.some(l => key(l) === probe.id && l.kind === kind)) {
        error(`unproven probe: ${probe.id}`);
      }
    }
    for (const [name, probe] of observedProbes) {
      if (!requiredNames.has(name)) error(`unexpected probe registration: ${probe.id}`);
    }
  }

  for (const [id] of observed) {
    if (selectedKeys.has(id)) continue;
    if (probes && probeIds.has(id)) continue;
    error(`unexpected registration: ${id}`);
  }

  if (!binaryUnchanged) error('target binary changed during the run');

  const counts = Object.fromEntries(['pass', 'fail', 'skip', 'todo'].map(s =>
    [s, [...observed.values()].filter(v => v === s).length]));
  return { failed, errors, counts, observed, validLaunches };
}

module.exports = { PROBE_FILE, REQUIRED_PROBES, checkPinnedLaunchForms, createLog, evaluateSuite, key, parentCredits, tsv, validLaunch };
