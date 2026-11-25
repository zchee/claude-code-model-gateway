import { readFileSync, writeFileSync } from 'node:fs';

const directory = new URL('../internal/jsonx/testdata/', import.meta.url);
const inputs = JSON.parse(readFileSync(new URL('node-inputs.json', directory), 'utf8'));
const golden = {};
for (const [name, { json, hex }] of Object.entries(inputs)) {
  const value = JSON.parse(hex === undefined ? json : Buffer.from(hex, 'hex').toString());
  golden[name] = { wire: JSON.stringify(value), state: JSON.stringify(value, null, 2) + '\n' };
}
const bytes = JSON.stringify(golden, null, 2) + '\n';
writeFileSync(new URL('node-golden.json', directory), bytes);
// The venue removes its checkout on exit. Return its exact file through the run log.
process.stdout.write(bytes);
