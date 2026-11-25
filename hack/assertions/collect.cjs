'use strict';
const {analyze, key} = require('./analyze.cjs');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {gzipSync} = require('node:zlib');
const {createHash} = require('node:crypto');
const [plugin, conformance, pin] = process.argv.slice(2);
const digest = bytes => createHash('sha256').update(bytes).digest('hex');
const input = name => fs.readFileSync(path.join(conformance, name), 'utf8');
const census = input('baseline/census.tsv'), overrides = input('assertion-overrides.tsv');
const columns = ['path','line','column','name','site_path','site_line','site_column'];
function rows(text) {
  assert(text.endsWith('\n'));
  const [header, ...lines] = text.slice(0,-1).split('\n').map(l => l.split('\t'));
  return lines.map(fields => {
    assert.equal(fields.length, header.length);
    return Object.fromEntries(header.map((h,i) => [h,fields[i]]));
  });
}
const cases = rows(census);
const baseline = JSON.parse(input('baseline/run.json'));
assert.equal(pin,key([baseline.commit,baseline.root_tree,baseline.plugin_tree]),'baseline and export pins differ');
const {pairs, sites, unresolved, resolvedCalls, productFiles, embeddedInputs} = analyze(plugin, cases);
const caseKeys = new Set(cases.map(r => key(columns.slice(0,4).map(c => r[c]))));
const applied = new Set();
for (const row of rows(overrides)) {
  const values = columns.map(c => row[c]), id = key(values);
  assert(caseKeys.has(key(values.slice(0,4))) && row.reason.trim() && !applied.has(id), `invalid override: ${id}`);
  applied.add(id);
  const site = sites.get(key(values.slice(4)));
  assert(site, `override does not point to a parsed assertion: ${id}`);
  if (row.action === 'add') {
    assert(!pairs.has(id), `stale add: ${id}`);
    pairs.set(id, [...values, site.call, 'override']);
  } else {
    assert(row.action === 'remove' && pairs.delete(id), `invalid removal: ${id}`);
  }
}
const sort = (a,b) => {
  for (const [i,x] of a.slice(0,7).entries()) {
    const y = b[i];
    const d = [1,2,5,6].includes(i) ? Number(x)-Number(y) : Buffer.compare(Buffer.from(String(x)),Buffer.from(String(y)));
    if (d) return d;
  }
  return 0;
};
const entries = [...pairs.values()].sort(sort);
for (const r of entries) assert(r.every(v => !/[\r\t\n]/.test(v)), `invalid TSV field: ${r}`);
const text = [[...columns,'call','via'],...entries].map(key).join('\n')+'\n';
const ruleSites = new Map();
for (const row of resolvedCalls) ruleSites.set(key(row.slice(4,8)),row[8]);
const rules = {};
for (const group of ruleSites.values()) rules[group] = (rules[group]??0)+1;
const present = new Set(entries.map(r => key(r.slice(4,7))));
const unreached = [...sites.keys()].filter(k => !present.has(k));
// A second, deliberately independent pass checks the assertion API spellings
// in this pin, including text in strings the outer parser sees only as data.
const unmatched = [];
for (const f of fs.readdirSync(path.join(plugin,'test'),{recursive:true}).filter(f => /\.[cm]?js$/.test(f))) {
  const file = 'test/'+f;
  for (const [i,line] of fs.readFileSync(path.join(plugin,file),'utf8').split('\n').entries()) {
    for (const m of line.matchAll(/\b(?:assert(?:\.\w+)?|require\(['"](?:node:)?assert(?:\/strict)?['"]\)\.\w+)\s*\(/g)) {
      const id = key([file,i+1,m.index+1]);
      if (!sites.has(id)) unmatched.push(id);
    }
  }
}
const openKeys = new Set(unresolved.map(row=>key(row.slice(4))));
const resolvedAtOpenSites = resolvedCalls.filter(row=>openKeys.has(key(row.slice(4,8))));
const payload = gzipSync(JSON.stringify({pin,census:digest(census),overrides:digest(overrides),
  pairs:text,unresolved,resolvedAtOpenSites,rules,productFiles,unreached,unmatched,embeddedInputs}));
process.stdout.write(`ASSERTION-SITES-V1 ${digest(payload)} ${payload.toString('base64')}\n`);
if (unreached.length || unmatched.length) {
  process.stderr.write(`incomplete attribution: ${unreached.length} unowned sites, ${unmatched.length} unmatched tokens; inspect the diagnostic frame\n`);
  process.exitCode = 1;
}
