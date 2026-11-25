import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

execFileSync(fileURLToPath(new URL('venue-check', import.meta.url)), { stdio: 'inherit' });
const require = createRequire(join(process.argv[2], 'package.json'));
const { adaptCodexToolSchemas, ToolSchemaCompatibilityError } = require('./lib/tool-schema-compat.js');
const Ajv2020 = require('./node_modules/ajv/dist/2020.js');
const ajv = require('./node_modules/ajv/package.json').version;
assert.equal(ajv, '8.20.0');

function verdicts(schema, instances) {
  const validate = new Ajv2020({ strict: false, unicodeRegExp: true }).compile(schema);
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

const directory = new URL('../internal/toolschema/testdata/', import.meta.url);
const inputs = JSON.parse(readFileSync(new URL('node-inputs.json', directory), 'utf8'));
const cases = {};
for (const [name, input] of Object.entries(inputs)) {
  const single = Object.hasOwn(input, 'schema');
  const tools = single ? [{ name: 'artifact_tool', input_schema: input.schema }] : input.tools;
  if (input.selfCycle) input.schema.self = input.schema;
  const snapshot = structuredClone(tools);
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
    if (input.instances) result.after = verdicts(output, input.instances);
  } catch (error) {
    if (!(error instanceof ToolSchemaCompatibilityError)) throw error;
    const { reasonCode, pointer, toolName } = error;
    result.error = { reasonCode, pointer, toolName };
  }
  assert.deepEqual(tools, snapshot, `${name}: input mutated`);
}
const bytes = JSON.stringify({ node: process.version, ajv, cases }, null, 2) + '\n';
writeFileSync(new URL('node-golden.json', directory), bytes);
// Successful remote runs discard the checkout. Return the same bytes on stdout.
process.stdout.write(bytes);
