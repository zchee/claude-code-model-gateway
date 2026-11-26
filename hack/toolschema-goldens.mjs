import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

execFileSync(fileURLToPath(new URL('venue-check', import.meta.url)), { stdio: 'inherit' });
assert.equal(process.version, 'v24.11.1');
const source = createRequire(join(process.argv[2], 'package.json'));
const deps = createRequire(join(process.argv[3], 'package.json'));
const { adaptCodexToolSchemas, ToolSchemaCompatibilityError } = source('./lib/tool-schema-compat.js');
const Ajv2020 = deps('./node_modules/ajv/dist/2020.js');
const ajv = deps('./node_modules/ajv/package.json').version;
assert.equal(ajv, '8.20.0');

function verdicts(schema, instances, expectInvalidRewrittenPattern = false) {
  const validator = new Ajv2020({ strict: false, unicodeRegExp: true });
  if (expectInvalidRewrittenPattern) {
    assert.throws(() => validator.compile(schema), error =>
      error instanceof SyntaxError && error.message.startsWith('Invalid regular expression: '));
    return { compiled: false };
  }
  const validate = validator.compile(schema);
  return instances.map(instance => validate(instance));
}

// The public adapter returns the rewritten tools, not its internal edit list.
function edits(before, after, pointer, result) {
  if (before === after) return;
  if (typeof before === 'string') {
    assert.equal(typeof after, 'string');
    result.push({ pointer, replacement: after });
    return;
  }
  assert.deepEqual(Object.keys(before), Object.keys(after));
  for (const key of Object.keys(before)) {
    const segment = key.replaceAll('~', '~0').replaceAll('/', '~1');
    edits(before[key], after[key], `${pointer}/${segment}`, result);
  }
}

const mode = process.argv[4] ?? 'toolschema-goldens';
assert.ok(['toolschema-goldens', 'toolschema-supplement'].includes(mode));
const supplement = mode === 'toolschema-supplement';
const directory = new URL('../internal/toolschema/testdata/', import.meta.url);
const inputFile = supplement ? 'node-supplement-inputs.json' : 'node-inputs.json';
let inputs = JSON.parse(readFileSync(new URL(inputFile, directory), 'utf8'));
assert.ok(Array.isArray(inputs) && inputs.length > 0, 'expected a nonempty array of cases');
const direct = Object.create(null);
if (supplement) {
  const adapters = [];
  for (const input of inputs) {
    const { name, note, pattern, repeat = 1, adapter = false } = input;
    assert.ok(Object.keys(input).every(key => ['name', 'note', 'pattern', 'repeat', 'adapter'].includes(key)), `${name}: unknown field`);
    assert.ok(typeof name === 'string' && name.length && !Object.hasOwn(direct, name), `empty or duplicate name: ${name}`);
    assert.ok(typeof note === 'string' && note.trim().length && typeof pattern === 'string', `${name}: expected a note and a pattern`);
    assert.ok(Number.isSafeInteger(repeat) && repeat > 0 && typeof adapter === 'boolean', `${name}: invalid repeat or adapter`);
    const expanded = pattern.repeat(repeat);
    try {
      new RegExp(expanded, 'u');
      direct[name] = 'valid';
    } catch (error) {
      if (!(error instanceof SyntaxError)) throw error;
      direct[name] = error.name;
    }
    if (adapter) adapters.push({ name, schema: { pattern: expanded } });
  }
  inputs = adapters;
}
const cases = Object.create(null);
for (const input of inputs) {
  assert.ok(input !== null && typeof input === 'object' && !Array.isArray(input), 'expected a case object');
  const { name } = input;
  assert.ok(typeof name === 'string' && name.length > 0, 'expected a nonempty case name');
  assert.ok(!Object.hasOwn(cases, name), `duplicate case: ${name}`);
  assert.ok(Object.keys(input).every(key => ['name', 'schema', 'tools', 'selfCycle', 'instances', 'expectInvalidRewrittenPattern'].includes(key)), `${name}: unknown case field`);
  const single = Object.hasOwn(input, 'schema');
  assert.notEqual(single, Object.hasOwn(input, 'tools'), `${name}: expected exactly one of schema and tools`);
  if (Object.hasOwn(input, 'instances')) assert.ok(single && Array.isArray(input.instances), `${name}: expected schema with instances array`);
  if (Object.hasOwn(input, 'expectInvalidRewrittenPattern')) {
    assert.ok(input.expectInvalidRewrittenPattern === true && Array.isArray(input.instances), `${name}: expected a true rewrite-failure marker and instances`);
  }
  if (Object.hasOwn(input, 'selfCycle')) {
    assert.ok(input.selfCycle === true && single && !input.instances && input.schema !== null && typeof input.schema === 'object'
      && !Array.isArray(input.schema) && !Object.hasOwn(input.schema, 'self'), `${name}: expected an object schema without self or instances`);
  }
  const tools = single ? [{ name: 'artifact_tool', input_schema: input.schema }] : input.tools;
  if (input.selfCycle) input.schema.self = input.schema;
  const snapshot = structuredClone(tools);
  const inputWire = input.selfCycle ? undefined : JSON.stringify(tools);
  const result = cases[name] = {};
  if (input.instances) result.before = verdicts(input.schema, input.instances);
  try {
    const adapted = adaptCodexToolSchemas(tools);
    const output = single ? adapted[0].input_schema : adapted;
    result.same = output === (single ? input.schema : tools);
    if (input.selfCycle) result.selfCycle = output.self === output;
    else result.wire = JSON.stringify(output);
    if (!single && Array.isArray(tools)) result.sameTools = adapted.map((tool, i) => tool === tools[i]);
    result.edits = [];
    edits(tools, adapted, '/tools', result.edits);
    if (input.instances) result.after = verdicts(output, input.instances, input.expectInvalidRewrittenPattern);
  } catch (error) {
    if (!(error instanceof ToolSchemaCompatibilityError) || input.expectInvalidRewrittenPattern) throw error;
    const { reasonCode, pointer, toolName } = error;
    result.error = { reasonCode, pointer, toolName };
  }
  assert.deepEqual(tools, snapshot, `${name}: input mutated`);
  if (!input.selfCycle) assert.equal(JSON.stringify(tools), inputWire, `${name}: serialized input mutated`);
}
assert.deepEqual(new Set(Object.keys(cases)), new Set(inputs.map(input => input.name)), 'input and output names differ');
const { v8, icu, unicode } = process.versions;
const corpus = supplement ? { node: process.version, v8, icu, unicode, ajv, direct, cases } : { node: process.version, ajv, cases };
const bytes = JSON.stringify(corpus, null, 2) + '\n';
writeFileSync(new URL(supplement ? 'node-supplement.json' : 'node-golden.json', directory), bytes);
// Successful remote runs discard the checkout. Return the same bytes on stdout.
process.stdout.write(bytes);
