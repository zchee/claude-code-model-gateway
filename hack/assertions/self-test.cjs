// Known case/site edges; the fixture itself must never be executed.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const {analyze} = require('./analyze.cjs');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'assertions-'));
try {
  fs.mkdirSync(path.join(dir, 'test'));
  fs.writeFileSync(path.join(dir, 'test/helper.js'), `const assert = require('node:assert/strict');
exports.shared = () => assert.ok(true);
`);
  for (const [filename,text] of Object.entries({
    'methods.mjs': `import {strictEqual as eq} from 'node:assert/strict';\nexport const checks = {verify(value = eq(1, 1)) { return value; }};\n`,
    'bridge.mjs': `export {checks} from './methods.mjs';\n`,
    'preload.cjs': `const assert = require('node:assert/strict');\nassert.ok(true);\n`,
    'imported.mjs': `import assert from 'node:assert/strict';\nassert['match']('x', /x/);\n`,
    'entry.cjs': `const assert = require('node:assert/strict');\nassert.equal(1, 1);\n`,
  })) fs.writeFileSync(path.join(dir,'test',filename),text);
  const source = `const test = require('node:test');
const assert = require('node:assert/strict');
const {shared} = require('./helper.js');
function run(cb) { cb(); }
test('a', t => { shared(); run(() => assert.fail('forbidden')); });
for (const name of ['b','c']) test(name, t => { shared(); });
test('parent', async t => { await t.test('child', () => assert.ok(1)); });
test('embedded', () => { require('node:child_process').spawnSync(process.execPath, ['-e', "const assert = require('node:assert/strict'); assert.equal(1, 2)"]); });
test('unknown', () => { globalThis[choose()](); globalThis[choose()].map(); require(choose()); });
test.after(() => assert.ok('cleanup'));
function spawnScript(script) { require('node:child_process').spawnSync(process.execPath, ['-e',script]); }
test('projected helper', () => spawnScript("require('./helper.js').shared()"));
test('imported callback', () => Promise.resolve().then(shared));
const {fail: forbidden} = require('node:assert/strict');
function guarded(pid, {kill}) { if (pid) kill(); return false; }
test('assertion values', t => {
  assert.equal(guarded(null, {kill: assert.fail}), false);
  const assigned = t.assert.ok;
  const provide = () => forbidden;
  [0].forEach(assigned);
  run(provide());
});
const {checks} = require('./bridge.mjs');
const cp = require('node:child_process');
const path = require('node:path');
const argv = () => ['--require', path.join(__dirname,'preload.cjs'), '--import', path.join(__dirname,'imported.mjs'), path.join(__dirname,'entry.cjs')];
test('default and method', () => checks.verify());
test('child flags', () => cp.spawn(process.execPath,argv()));
test('fork entry', () => cp.fork(path.join(__dirname,'entry.cjs')));
test('unknown entry', () => cp.fork(choose()));
function launchOptions(options) { cp.spawn(process.execPath, ['-e', ''], options); }
function launchEnvironment(env) { launchOptions({env}); }
test('variable preload', () => {
  const env = {NODE_OPTIONS: '--require=' + JSON.stringify(path.join(__dirname,'preload.cjs'))};
  launchEnvironment(env);
});
test('unknown environment', () => launchEnvironment(choose()));
test('preload already owned', () => cp.fork(path.join(__dirname,'preload.cjs')));
test('options wrapper', () => {
  const options = () => ({env:{NODE_OPTIONS:'--import=' + path.join(__dirname,'imported.mjs')}});
  launchOptions(options());
});
test('opaque options', () => launchOptions(choose()));
test('bound execArgv', () => {
  const flags = ['-r',path.join(__dirname,'preload.cjs')];
  const options = {execArgv:flags,env:{}};
  cp.fork(path.join(__dirname,'entry.cjs'),options);
});
test('opaque execArgv', () => cp.fork(path.join(__dirname,'entry.cjs'),{execArgv:choose(),env:{}}));
test('second shared launch', () => {
  launchEnvironment({});
  const env = {NODE_OPTIONS: \`--require=\${JSON.stringify(path.join(__dirname,'preload.cjs'))}\`};
  launchEnvironment(env);
});
test('spread then explicit preload', () => {
  const env = {...choose(), NODE_OPTIONS: '--require=' + path.join(__dirname,'preload.cjs')};
  launchEnvironment(env);
});
test('opaque final spread', () => {
  const env = {NODE_OPTIONS:'--require=' + path.join(__dirname,'preload.cjs'), ...choose()};
  launchEnvironment(env);
});
test('opaque argv', () => cp.spawn(process.execPath,choose(),{env:{}}));
`;
  fs.writeFileSync(path.join(dir, 'test/example.test.js'), source);
  const lines = source.split('\n');
  const cases = [['a',5],['b',6],['c',6],['parent',7],['parent > child',7],['embedded',8],['unknown',9],['projected helper',12],['imported callback',13],['assertion values',16],['default and method',27],['child flags',28],['fork entry',29],['unknown entry',30],['variable preload',33],['unknown environment',37],['preload already owned',38],['options wrapper',39],['opaque options',43],['bound execArgv',44],['opaque execArgv',49],['second shared launch',50],['spread then explicit preload',55],['opaque final spread',59],['opaque argv',63]].map(([name,line]) => ({
    path: 'test/example.test.js', name, line, column: lines[line-1].indexOf(name.includes(' > ') ? '.test(' : 'test(') + (name.includes(' > ') ? 2 : 1)}));
  const {pairs, unresolved} = analyze(dir, cases);
  const got = [...pairs.values()].map(r => [r[3],r[4],r[5],r[6],r[7]]);
  const expected = cases.map(r => [r.name,r.path,10,'assert.ok']);
  for (const name of ['a','b','c','projected helper','imported callback']) expected.push([name,'test/helper.js',2,'assert.ok']);
  expected.push(['a',cases[0].path,5,'assert.fail'], ['parent',cases[0].path,7,'assert.ok'], ['parent > child',cases[0].path,7,'assert.ok'], ['embedded',cases[0].path,8,'assert.equal']);
  for (const [line,call] of [[17,'assert.equal'],[17,'assert.fail'],[18,'t.assert.ok'],[19,'forbidden'],[20,'assigned']]) {
    expected.push(['assertion values',cases[0].path,line,call]);
  }
  const handed = [...pairs.values()].find(r=>r[3]==='assertion values' && r[7]==='assert.fail');
  assert.equal(handed?.[6],lines[16].indexOf('assert.fail')+1);
  assert.equal(handed?.[8],'callback:kill');
  expected.push(['default and method','test/methods.mjs',2,'eq'], ['child flags','test/preload.cjs',2,'assert.ok'],
    ['child flags','test/imported.mjs',2,"assert['match']"], ['child flags','test/entry.cjs',2,'assert.equal'], ['fork entry','test/entry.cjs',2,'assert.equal']);
  for (const name of ['variable preload','preload already owned','bound execArgv','second shared launch','spread then explicit preload','opaque final spread']) expected.push([name,'test/preload.cjs',2,'assert.ok']);
  expected.push(['options wrapper','test/imported.mjs',2,"assert['match']"],['bound execArgv','test/entry.cjs',2,'assert.equal'],['opaque execArgv','test/entry.cjs',2,'assert.equal']);
  const withColumns = expected.map(([name,file,line,call])=>{
    const sourceLine=fs.readFileSync(path.join(dir,file),'utf8').split('\n')[line-1];
    const start=sourceLine.indexOf(call);
    assert(start>=0 && start===sourceLine.lastIndexOf(call),`ambiguous fixture position: ${file}:${line} ${call}`);
    return [name,file,line,start+1,call];
  });
  assert.deepEqual(got.sort(),withColumns.sort());
  assert(unresolved.some(r=>r[3]==='unknown entry' && r.at(-1)==='child-launch:fork'));
  for (const name of ['unknown environment','opaque options','opaque execArgv','opaque final spread','opaque argv']) assert(unresolved.some(r=>r[3]===name && r.at(-1).startsWith('child-launch:')),name);
  for (const name of ['variable preload','options wrapper','bound execArgv','second shared launch','spread then explicit preload']) assert(!unresolved.some(r=>r[3]===name && r.at(-1).startsWith('child-launch:')),name);
  assert(unresolved.some(r => r[3] === 'unknown' && r.at(-1) === 'globalThis[choose()]'));
  assert(unresolved.some(r => r[3] === 'unknown' && r.at(-1) === 'globalThis[choose()].map'));
  assert(unresolved.some(r => r[3] === 'unknown' && r.at(-1) === 'require'));
  console.info(`assertion inventory self-test: ${cases.length} case identities, ${got.length} expected pairs, unresolved call detected`);
} finally { fs.rmSync(dir, {recursive: true, force: true}); }
