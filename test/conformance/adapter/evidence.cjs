// Copyright 2026 The claude-code-model-gateway Authors.
// SPDX-License-Identifier: Apache-2.0
'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

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
const validLaunch = (r, target, build) => r.target === target && r.build === build && Number.isSafeInteger(r.pid) && r.pid > 0;

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

module.exports = { createLog, key, parentCredits, tsv, validLaunch };
