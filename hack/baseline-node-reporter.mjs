// Copyright 2026 The claude-code-model-gateway Authors.
// SPDX-License-Identifier: Apache-2.0

import { relative } from 'node:path';

// Keep the runner's own events rather than arbitrary child stdout or diagnostics.
// Only diagnostic frames include the full failure; the TSV uses its first line.
export default async function* census(source) {
  yield JSON.stringify({ type: 'baseline:environment',
    XDG_RUNTIME_DIR: Object.hasOwn(process.env, 'XDG_RUNTIME_DIR'),
    DBUS_SESSION_BUS_ADDRESS: Object.hasOwn(process.env, 'DBUS_SESSION_BUS_ADDRESS') }) + '\n';
  for await (const { type, data } of source) {
    const file = data.file && relative(process.cwd(), data.file);
    if (type === 'test:summary') {
      yield JSON.stringify({ type, file, success: data.success, counts: data.counts }) + '\n';
    } else if (['test:start', 'test:pass', 'test:fail'].includes(type)) {
      const { name, line, column, nesting, skip, todo } = data;
      const error = data.details?.error;
      yield JSON.stringify({ type, file, name, line, column, nesting, skip, todo,
        failure: error?.cause?.stack ?? error?.stack ?? error?.message,
        failureType: error?.failureType, error: error?.message?.split(/[\r\n]/, 1)[0] }) + '\n';
    }
  }
}
