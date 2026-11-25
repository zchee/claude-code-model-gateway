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
`;
  fs.writeFileSync(path.join(dir, 'test/example.test.js'), source);
  const lines = source.split('\n');
  const cases = [['a',5],['b',6],['c',6],['parent',7],['parent > child',7],['embedded',8],['unknown',9],['projected helper',12],['imported callback',13]].map(([name,line]) => ({
    path: 'test/example.test.js', name, line, column: lines[line-1].indexOf(name.includes(' > ') ? '.test(' : 'test(') + (name.includes(' > ') ? 2 : 1)}));
  const {pairs, unresolved} = analyze(dir, cases);
  const got = [...pairs.values()].map(r => [r[3],r[4],r[5],r[7]]);
  const expected = cases.map(r => [r.name,r.path,10,'assert.ok']);
  for (const name of ['a','b','c','projected helper','imported callback']) expected.push([name,'test/helper.js',2,'assert.ok']);
  expected.push(['a',cases[0].path,5,'assert.fail'], ['parent',cases[0].path,7,'assert.ok'], ['parent > child',cases[0].path,7,'assert.ok'], ['embedded',cases[0].path,8,'assert.equal']);
  assert.deepEqual(got.sort(),expected.sort());
  assert(unresolved.some(r => r[3] === 'unknown' && r.at(-1) === 'globalThis[choose()]'));
  assert(unresolved.some(r => r[3] === 'unknown' && r.at(-1) === 'globalThis[choose()].map'));
  assert(unresolved.some(r => r[3] === 'unknown' && r.at(-1) === 'require'));
  console.info(`assertion inventory self-test: ${cases.length} case identities, ${got.length} expected pairs, unresolved call detected`);
} finally { fs.rmSync(dir, {recursive: true, force: true}); }
