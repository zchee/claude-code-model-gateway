// Copyright 2026 The claude-code-model-gateway Authors.
// SPDX-License-Identifier: Apache-2.0
'use strict';

const fs = require('node:fs');
const path = require('node:path');
const cp = require('node:child_process');
const { createHook, executionAsyncId } = require('node:async_hooks');
const { getCallSites } = require('node:util');
const { tsv } = require('./evidence.cjs');

module.exports = function install(config, inherited) {
  // --require propagates from the runner to its test files, not to gateway
  // children. Do not instrument the runner, npm, fixture proxies or fake CLIs.
  if (!inherited && !process.argv[1]?.endsWith('.test.js')) return;
  const { plugin, root, target, build, log } = config;
  if (!inherited && process.execArgv.includes('--test')) return;
  if (!inherited && config.trackActive !== false) {
    const running = path.join(config.work, `active-${process.pid}`);
    fs.writeFileSync(running, path.relative(plugin, process.argv[1]), { flag: 'wx' });
    process.on('exit', () => fs.rmSync(running, { force: true }));
  }
  const cli = path.join(plugin, 'bin/model-gateway.js');
  const cliBytes = fs.readFileSync(cli);
  const sites = new Map();
  let birthSeq = 0;
  for (const { path: file, line, kind, via, case: name } of tsv(path.join(root, 'test/conformance/launch-sites.tsv'))) {
    const key = `${file}:${line}`;
    if (!sites.has(key)) sites.set(key, []);
    sites.get(key).push({ site: key, kind, via, name });
  }

  // The pinned node:test runner uses the Test async resource and trigger IDs
  // for this same lookup. Do not wrap test() and move its registration site.
  const owners = new Map();
  if (!inherited) createHook({
    init(id, type, trigger, resource) {
      const owner = type === 'Test' ? resource : owners.get(trigger);
      if (owner) owners.set(id, owner);
    },
    destroy(id) { owners.delete(id); },
  }).enable();

  function owner() {
    if (inherited) return inherited.owner;
    const resource = owners.get(executionAsyncId());
    const test = resource?.parentTest ?? resource;
    if (!test?.parent || !test.loc) return null;
    const names = [];
    for (let t = test; t?.parent; t = t.parent) names.unshift(t.name);
    const file = test.loc.file;
    return { path: path.relative(file.startsWith(plugin + '/') ? plugin : root, file),
      line: test.loc.line, column: test.loc.column, name: names.join(' > ') };
  }

  const sync = cp.spawnSync;
  function dispatch(fn, selection, command, args, binaryArgs, options) {
    return selection === 'identity' ? fn(command, args, options) : fn(selection.slice(7), binaryArgs, options);
  }
  function witness(command, args, binaryArgs, options) {
    const env = options?.env ?? process.env;
    const result = dispatch(sync, `binary:${config.witness}`, command, args, binaryArgs,
      { env, stdio: ['ignore', 'pipe', 'pipe'], timeout: 5000 });
    if (result.status !== 0) throw new Error('recording binary failed to run');
    const text = result.stdout.toString();
    const end = text.indexOf('\n');
    const count = Number(text.slice(0, end));
    const fields = text.slice(end + 1, -1).split('\0');
    const expectedEnv = Object.entries(env).filter(([, v]) => v !== undefined).map(([k, v]) => `${k}=${v}`).sort();
    if (!text.endsWith('\0') || count !== binaryArgs.length || JSON.stringify(binaryArgs) !== JSON.stringify(fields.slice(0, count)) ||
        JSON.stringify(expectedEnv) !== JSON.stringify(fields.slice(count).sort())) throw new Error('recording binary observed different argv or environment');
    return result.pid;
  }
  function failClosed(reason, kind, testInfo) {
    fs.appendFileSync(log, JSON.stringify({
      ...(testInfo ?? {}),
      unattributed: true,
      reason,
      kind,
      target,
      build,
    }) + '\n');
    throw new Error(`conformance adapter: ${reason}`);
  }

  for (const method of ['spawn', 'spawnSync']) {
    const original = cp[method];
    cp[method] = function (command, args, options) {
      if (command !== process.execPath || !Array.isArray(args)) return original.apply(this, arguments);

      let binaryArgs, kind, isCli = false;
      const entry = args[0];
      const script = entry === '--require' || entry === '-r' ? 2 : 0;
      const arg = args[script];
      let resolved = null;
      if (arg && !arg.startsWith('-')) {
        const candidate = path.resolve(options?.cwd ?? process.cwd(), arg);
        try {
          if (fs.statSync(candidate).isFile()) resolved = fs.realpathSync.native(candidate);
        } catch { /* Non-existent or unreadable script is not a product entry. */ }
      }
      const isBirthObserver = !inherited && arg && path.basename(arg) === 'worker-birth.cjs';
      if (resolved === cli || (resolved && resolved.endsWith('/bin/model-gateway.js') && fs.readFileSync(resolved).equals(cliBytes))) {
        isCli = true;
        binaryArgs = args.slice(script + 1);
        kind = binaryArgs[0] === 'serve-shim' ? 'supervisor' : binaryArgs[0] === 'serve-worker' ? 'worker' : 'cli';
      } else if (entry === '-e' && args[1] === `require(${JSON.stringify(path.join(plugin, 'lib/request-worker.js'))}).runWorker()`) {
        kind = 'worker'; binaryArgs = ['serve-worker'];
      } else if (resolved && resolved.endsWith('/model-gateway/model-gateway.js')) {
        kind = 'shim'; binaryArgs = args.slice(1);
      } else if (resolved && resolved.endsWith('/model-gateway/update.js')) {
        kind = 'shim'; binaryArgs = ['setup', '--preserve-wiring'];
      } else if (resolved === path.join(plugin, 'hooks/registry-writer.js')) {
        kind = 'hook'; binaryArgs = ['hook', 'session-start', ...args.slice(1)];
      } else if (resolved === path.join(plugin, 'hooks/ensure.js')) {
        kind = 'hook'; binaryArgs = args.slice(1);
      } else if (!isBirthObserver) {
        // Known decoys, fake fixtures and unrelated Node scripts stay on Node.
        return original.apply(this, arguments);
      }

      const detectedKind = kind ?? 'birth';
      const test = owner();
      if (!test) failClosed(`product launch (${detectedKind}) without test owner`, detectedKind, null);

      const matches = inherited ? [inherited.row] : getCallSites(100).flatMap(frame =>
        sites.get(`${path.relative(plugin, frame.scriptName)}:${frame.lineNumber}`) ?? []);
      // V8's async stack also contains callers suspended at await. getCallSites
      // alone stops at the helper that resumed after allocating a fixture port.
      if (!inherited) {
        const limit = Error.stackTraceLimit;
        let stack;
        try { Error.stackTraceLimit = 100; stack = new Error().stack; }
        finally { Error.stackTraceLimit = limit; }
        for (const line of stack.split('\n')) {
          const location = /(?:\(|\s)(\/.*):(\d+):\d+\)?$/.exec(line);
          if (location) matches.push(...(sites.get(`${path.relative(plugin, location[1])}:${location[2]}`) ?? []));
        }
      }
      if (!matches.length) {
        failClosed(`product launch (${detectedKind}) outside inventoried sites for ${test.path}:${test.line}`, detectedKind, test);
      }

      const owned = matches.filter(r => r.name === test.name && !r.via.startsWith('subtest:'));
      // This generated birth observer itself must stay Node. Its inner spawn
      // is a copied gateway entry, at a source line embedded in a string.
      if (isBirthObserver) {
        if (!owned.some(r => r.via === 'startStaleWorkerFixture')) {
          failClosed(`unattributed worker birth observer for ${test.path}:${test.line}`, 'birth', test);
        }
        const inner = [...sites.values()].flat().find(r => r.name === test.name && r.kind === 'worker' && r.via === 'spawn');
        if (!inner) failClosed(`missing worker birth launch site for ${test.path}:${test.line}`, 'birth', test);
        const preload = path.join(config.work, `birth-${process.pid}-${++birthSeq}.cjs`);
        fs.writeFileSync(preload, `require(${JSON.stringify(__filename)})(${JSON.stringify(config)},${JSON.stringify({ owner: test, row: inner })});\n`);
        return original.call(this, command, ['--require', preload, ...args], options);
      }

      const allowSupportHelper = test.path.startsWith('test/conformance/');
      const row = owned.find(r => r.kind === kind) ??
        matches.find(r => r.name === '-' && r.kind === kind) ??
        (isCli && allowSupportHelper && matches.find(r => r.site.startsWith('test/support.js:') && r.kind === 'helper'));
      if (!row) {
        failClosed(`product launch (${kind}) has no matching inventory row for ${test.path}:${test.line} ${test.name}`, kind, test);
      }

      const witnessPid = config.witness ? witness(command, args, binaryArgs, options) : null;
      const child = dispatch(original, target, command, args, binaryArgs, options);
      if (Number.isInteger(child.pid) && child.pid > 0) {
        const callers = [...new Set(matches.filter(r => r.name === test.name && r.kind === kind && r.site !== row.site).map(r => r.site))];
        fs.appendFileSync(log, JSON.stringify({ ...test, site: row.site, kind, target, pid: child.pid, build,
          ...(callers.length ? { callers } : {}),
          ...(witnessPid ? { witnessPid } : {}) }) + '\n');
      }
      return child;
    };
  }
};
