// The entrypoint the grader runs (node /bot/main.ts). The forge itself is a statically linked Go binary
// built from go/ and committed under bin/ (bin/forge-<platform>-<arch>): it binds its port, registers
// with the env, serves the suite and exits 0 once the env says done. This file starts it, passes the
// environment through untouched, and exits with its code.
//
// With no binary for this platform (a checkout on another OS, or a build not committed yet), the Node
// implementation in node-forge.ts runs instead. The choice is made before anything registers, so the
// env only ever sees one forge.

import { spawn } from 'node:child_process';
import { accessSync, constants } from 'node:fs';
import { join } from 'node:path';

const HERE = import.meta.dirname;
const BINARY = process.env.FORGE_BIN ?? join(HERE, 'bin', `forge-${process.platform}-${process.arch}`);

function runnable(path: string): boolean {
  try {
    accessSync(path, constants.X_OK);
    return true;
  } catch {
    return false;
  }
}

if (process.env.FORGE_IMPL !== 'node' && runnable(BINARY)) {
  const child = spawn(BINARY, [], { stdio: 'inherit', env: process.env });
  for (const signal of ['SIGTERM', 'SIGINT'] as const) process.on(signal, () => child.kill(signal));
  child.on('error', (err) => {
    process.stderr.write(`[forge] could not start ${BINARY}: ${err.message}\n`);
    process.exit(1);
  });
  child.on('exit', (code, signal) => process.exit(code ?? (signal === null ? 1 : 0)));
} else {
  await import('./node-forge.ts');
}
