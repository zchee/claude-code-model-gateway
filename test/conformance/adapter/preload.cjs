// Copyright 2026 The claude-code-model-gateway Authors.
// SPDX-License-Identifier: Apache-2.0
'use strict';

const fs = require('node:fs');
const path = require('node:path');
const cp = require('node:child_process');
const { createHook, executionAsyncId } = require('node:async_hooks');
const { getCallSites } = require('node:util');
const { tsv } = require('./evidence.cjs');

const NODE_VALUE_FLAGS = new Set([
  '-r', '--require', '--import', '--loader', '--experimental-loader',
  '-C', '--conditions', '--input-type', '--title',
  '--env-file', '--env-file-if-exists', '--redirect-warnings',
  '--diagnostic-dir', '--test-reporter', '--test-reporter-destination',
  '--test-name-pattern', '--max-old-space-size', '--heapsnapshot-near-heap-limit',
  '--cpu-prof-dir', '--cpu-prof-name', '--heap-prof-dir', '--heap-prof-name',
  '--inspect', '--inspect-brk', '--inspect-port', '--openssl-config',
  '--icu-data-dir', '--policy-integrity', '--experimental-policy',
  '--experimental-sea-config', '--report-directory', '--report-filename',
  '--report-signal', '--heapsnapshot-signal', '--secure-heap', '--secure-heap-min',
  '--tls-cipher-list', '--tls-keylog', '--v8-pool-size', '--watch-path',
  '--allow-fs-read', '--allow-fs-write',
]);

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
  const execReal = fs.realpathSync.native(process.execPath);
  const cli = path.join(plugin, 'bin/model-gateway.js');
  const cliBytes = fs.readFileSync(cli);
  const workerPath = path.join(plugin, 'lib/request-worker.js');
  const workerEvalExact = `require(${JSON.stringify(workerPath)}).runWorker()`;
  const hookRegistry = path.join(plugin, 'hooks/registry-writer.js');
  const hookEnsure = path.join(plugin, 'hooks/ensure.js');

  const sites = new Map();
  let birthSeq = 0;
  let inAdapter = false;
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

  function resolveRealFile(arg, cwd) {
    if (typeof arg !== 'string' || !arg) return null;
    // Preserve symlink/.. order; lexical normalization changes kernel lookup.
    const candidate = path.isAbsolute(arg) ? arg : `${cwd}/${arg}`;
    try {
      if (fs.statSync(candidate).isFile()) return fs.realpathSync.native(candidate);
    } catch { /* Non-existent or unreadable path is not a product entry. */ }
    return null;
  }

  function classifyProductScript(resolved, rawArg) {
    if (!inherited && typeof rawArg === 'string' && path.basename(rawArg) === 'worker-birth.cjs') {
      return { type: 'birth', kind: 'birth', isCli: false, isBirthObserver: true };
    }
    if (!resolved) return null;
    if (resolved === cli || (resolved.endsWith('/bin/model-gateway.js') && fs.readFileSync(resolved).equals(cliBytes))) {
      return { type: 'cli', kind: null, isCli: true, isBirthObserver: false };
    }
    if (resolved.endsWith('/model-gateway/model-gateway.js')) {
      return { type: 'shim-cli', kind: 'shim', isCli: false, isBirthObserver: false };
    }
    if (resolved.endsWith('/model-gateway/update.js')) {
      return { type: 'shim-update', kind: 'shim', isCli: false, isBirthObserver: false };
    }
    if (resolved === hookRegistry) {
      return { type: 'hook-registry', kind: 'hook', isCli: false, isBirthObserver: false };
    }
    if (resolved === hookEnsure) {
      return { type: 'hook-ensure', kind: 'hook', isCli: false, isBirthObserver: false };
    }
    return null;
  }

  function executable(cmd, cwd, options) {
    if (typeof cmd !== 'string' || !cmd) return null;
    const search = (options?.env ?? process.env).PATH ?? '/usr/bin:/bin';
    const candidates = cmd.includes('/') ? [cmd] : search.split(path.delimiter).map(d => `${d || '.'}/${cmd}`);
    for (const candidate of candidates) {
      const real = resolveRealFile(candidate, cwd);
      if (!real) continue;
      try { fs.accessSync(real, fs.constants.X_OK); return real; }
      catch (err) { if (err.code !== 'EACCES' && err.code !== 'ENOENT') throw err; }
    }
    return null;
  }

  function parseNodeArgs(nodeArgs) {
    const nodeOptions = [];
    let scriptIndex = -1;
    let evalCode = null;
    let printCode = null;
    for (let i = 0; i < nodeArgs.length; i++) {
      const a = nodeArgs[i];
      if (a === '--') {
        if (i + 1 < nodeArgs.length) scriptIndex = i + 1;
        break;
      }
      if (a === '-e' || a === '--eval') {
        evalCode = nodeArgs[i + 1] ?? '';
        break;
      }
      if (a.startsWith('--eval=')) {
        evalCode = a.slice('--eval='.length);
        break;
      }
      if (a === '-p' || a === '--print') {
        printCode = nodeArgs[i + 1] ?? '';
        break;
      }
      if (a.startsWith('--print=')) {
        printCode = a.slice('--print='.length);
        break;
      }
      if (NODE_VALUE_FLAGS.has(a)) {
        nodeOptions.push({ flag: a, value: nodeArgs[i + 1] ?? '' });
        i++;
        continue;
      }
      if (a.startsWith('-')) {
        const eq = a.indexOf('=');
        nodeOptions.push(eq === -1 ? { flag: a, value: null } : { flag: a.slice(0, eq), value: a.slice(eq + 1) });
        continue;
      }
      scriptIndex = i;
      break;
    }
    return { nodeOptions, scriptIndex, evalCode, printCode };
  }

  function isAllowedOptionsForProduct(prod, nodeOptions, cwd) {
    if (!nodeOptions.length) return true;
    const [opt] = nodeOptions;
    if (!prod.isCli || nodeOptions.length !== 1 || opt.flag !== '--require') return false;
    const fixture = resolveRealFile(opt.value, cwd);
    if (!fixture || fixture !== `${fs.realpathSync.native(cwd)}/delay-proxy-startup.cjs`) return false;
    const modulePath = JSON.stringify(path.join(plugin, 'lib/process-supervision.js'));
    const body = fs.readFileSync(fixture, 'utf8');
    return body.includes(modulePath) && require('node:crypto').createHash('sha256')
      .update(body.replace(modulePath, 'PINNED_MODULE')).digest('hex') ===
      'e77c87c51bba3a2a66d6df55d2e2fd64e90b6a85ad437b64d560bbfa518eb25b';
  }

  function productInvocationFromScript(prod, tailArgs) {
    if (prod.isBirthObserver) {
      return { kind: 'birth', binaryArgs: null, isCli: false, isBirthObserver: true };
    }
    if (prod.type === 'cli') {
      const kind = tailArgs[0] === 'serve-shim' ? 'supervisor' : tailArgs[0] === 'serve-worker' ? 'worker' : 'cli';
      return { kind, binaryArgs: tailArgs, isCli: true, isBirthObserver: false };
    }
    if (prod.type === 'shim-cli') {
      return { kind: 'shim', binaryArgs: tailArgs, isCli: false, isBirthObserver: false };
    }
    if (prod.type === 'shim-update') {
      return { kind: 'shim', binaryArgs: ['setup', '--preserve-wiring'], isCli: false, isBirthObserver: false };
    }
    if (prod.type === 'hook-registry') {
      return { kind: 'hook', binaryArgs: ['hook', 'session-start', ...tailArgs], isCli: false, isBirthObserver: false };
    }
    if (prod.type === 'hook-ensure') {
      return { kind: 'hook', binaryArgs: tailArgs, isCli: false, isBirthObserver: false };
    }
    return null;
  }

  function inspectInvocation(command, args, options) {
    if (options?.shell) return null;
    const cwd = options?.cwd ?? process.cwd();
    const argList = Array.isArray(args) ? args : [];
    const program = executable(command, cwd, options);
    const directProd = classifyProductScript(program, program);
    if (directProd) {
      const info = productInvocationFromScript(directProd, argList);
      return { unsupported: true, kind: info.kind, reason: `unsupported direct product execution (${info.kind})` };
    }

    let effCmd = program;
    let effArgs = argList;
    if (program === '/usr/bin/env') {
      const index = argList[0] === '--' ? 1 : 0;
      if (!argList[index] || argList[index].startsWith('-') || argList[index].includes('=')) return null;
      effCmd = executable(argList[index], cwd, options);
      effArgs = argList.slice(index + 1);
    }
    if (effCmd !== execReal) return null;

    const parsed = parseNodeArgs(effArgs);
    if (parsed.evalCode !== null) {
      if (parsed.evalCode === workerEvalExact) {
        if (parsed.nodeOptions.length > 0) {
          return { unsupported: true, kind: 'worker', reason: `unsupported Node options before worker eval: ${parsed.nodeOptions.map(o => o.flag).join(' ')}` };
        }
        return { kind: 'worker', binaryArgs: ['serve-worker'], isCli: false, isBirthObserver: false };
      }
      return null;
    }

    if (parsed.printCode !== null) {
      return null;
    }

    if (parsed.scriptIndex !== -1) {
      const scriptArg = effArgs[parsed.scriptIndex];
      const prod = classifyProductScript(resolveRealFile(scriptArg, cwd), scriptArg);
      if (prod) {
        const info = productInvocationFromScript(prod, effArgs.slice(parsed.scriptIndex + 1));
        if (!isAllowedOptionsForProduct(prod, parsed.nodeOptions, cwd)) {
          return { unsupported: true, kind: info.kind, reason: `unsupported Node options before product entry (${info.kind}): ${parsed.nodeOptions.map(o => o.flag).join(' ')}` };
        }
        return info;
      }
    }

    return null;
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
      if (inAdapter) return original.apply(this, arguments);
      const effOptions = Array.isArray(args) ? options : (args && typeof args === 'object' ? args : undefined);
      const effArgs = Array.isArray(args) ? args : [];
      const detected = inspectInvocation(command, effArgs, effOptions);
      if (!detected) {
        // Known decoys, fake fixtures and unrelated scripts stay on Node/OS.
        return original.apply(this, arguments);
      }
      const test = owner();
      if (detected.unsupported) {
        failClosed(detected.reason, detected.kind, test);
      }

      const { kind, binaryArgs, isCli, isBirthObserver } = detected;
      if (!test) failClosed(`product launch (${kind}) without test owner`, kind, null);

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
        failClosed(`product launch (${kind}) outside inventoried sites for ${test.path}:${test.line}`, kind, test);
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
        inAdapter = true;
        try {
          return original.call(this, command, ['--require', preload, ...effArgs], effOptions);
        } finally {
          inAdapter = false;
        }
      }

      const allowSupportHelper = test.path.startsWith('test/conformance/');
      const row = owned.find(r => r.kind === kind) ??
        matches.find(r => r.name === '-' && r.kind === kind) ??
        (isCli && allowSupportHelper && matches.find(r => r.site.startsWith('test/support.js:') && r.kind === 'helper'));
      if (!row) {
        failClosed(`product launch (${kind}) has no matching inventory row for ${test.path}:${test.line} ${test.name}`, kind, test);
      }

      inAdapter = true;
      let witnessPid = null;
      let child;
      try {
        witnessPid = config.witness ? witness(command, effArgs, binaryArgs, effOptions) : null;
        child = dispatch(original, target, command, effArgs, binaryArgs, effOptions);
      } finally {
        inAdapter = false;
      }
      if (Number.isInteger(child.pid) && child.pid > 0) {
        const callers = [...new Set(matches.filter(r => r.name === test.name && r.kind === kind && r.site !== row.site).map(r => r.site))];
        fs.appendFileSync(log, JSON.stringify({ ...test, site: row.site, kind, target, pid: child.pid, build,
          ...(callers.length ? { callers } : {}),
          ...(witnessPid ? { witnessPid } : {}) }) + '\n');
      }
      return child;
    };
  }

  for (const method of ['execFile', 'execFileSync']) {
    const original = cp[method];
    cp[method] = function (file, args, options) {
      if (!inAdapter) {
        const effArgs = Array.isArray(args) ? args : [];
        const effOptions = Array.isArray(args) ? options : (args && typeof args === 'object' ? args : undefined);
        const detected = inspectInvocation(file, effArgs, effOptions);
        if (detected) {
          failClosed(`unsupported product invocation via child_process.${method} (${detected.kind})`, detected.kind, owner());
        }
      }
      return original.apply(this, arguments);
    };
  }

  const originalFork = cp.fork;
  cp.fork = function (modulePath, args, options) {
    if (!inAdapter) {
      const effOptions = Array.isArray(args) ? options : (args && typeof args === 'object' ? args : undefined);
      const cwd = effOptions?.cwd ?? process.cwd();
      const prod = classifyProductScript(resolveRealFile(modulePath, cwd), modulePath);
      if (prod) {
        const info = productInvocationFromScript(prod, Array.isArray(args) ? args : []);
        failClosed(`unsupported product invocation via child_process.fork (${info.kind})`, info.kind, owner());
      }
    }
    return originalFork.apply(this, arguments);
  };
};
