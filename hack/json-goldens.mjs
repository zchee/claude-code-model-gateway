import { readFileSync, writeFileSync } from 'node:fs';

const directory = new URL('../internal/jsonx/testdata/', import.meta.url);
const inputs = JSON.parse(readFileSync(new URL('node-inputs.json', directory), 'utf8'));
const golden = {};
for (const [name, { json, hex, framing }] of Object.entries(inputs)) {
  const value = JSON.parse(hex === undefined ? json : Buffer.from(hex, 'hex').toString());
  golden[name] = { wire: JSON.stringify(value), state: JSON.stringify(value, null, 2) + '\n' };
  if (framing === 'compact') golden[name].file = JSON.stringify(value);
  if (framing === 'line') golden[name].file = JSON.stringify(value) + '\n';
}

const array = depth => '['.repeat(depth) + '0' + ']'.repeat(depth);
// Do not seek a heap-exhaustion boundary on the shared venue. A success at
// the ceiling is a measured lower bound, never an asserted runtime maximum.
const ceiling = 65536;
function measure(stringify) {
  let low = 0, high = ceiling + 1;
  while (high - low > 1) {
    const n = Math.floor((low + high) / 2);
    const input = array(n);
    try {
      const parsed = JSON.parse(input);
      if (stringify && JSON.stringify(parsed) !== input) throw new Error('round trip mismatch');
      low = n;
    } catch (err) {
      if (!(err instanceof RangeError)) throw err;
      high = n;
    }
  }
  return low;
}
const depth = {node: process.version, ceiling, parse: measure(false), roundTrip: measure(true)};
const shared = Math.min(depth.parse, depth.roundTrip, 10000);
depth.shared = shared;
depth.wire = JSON.stringify(JSON.parse(array(shared)));
const bytes = JSON.stringify({cases: golden, depth}, null, 2) + '\n';
writeFileSync(new URL('node-golden.json', directory), bytes);
// The venue removes its checkout on exit. Return its exact file through the run log.
process.stdout.write(bytes);
