// The Node implementation of the forge, kept as the fallback main.ts starts when no prebuilt Go binary
// matches this platform: start the server on an ephemeral port, register that port with the env, then
// poll the env until it says the trial is done and exit 0.
import { createApp } from './app.ts';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
const REQUEST_TIMEOUT_MS = 30_000;
const RETRY_INITIAL_MS = 100;
const RETRY_MAX_MS = 1_000;

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));
const retryDelay = (attempt: number): number => Math.min(RETRY_MAX_MS, RETRY_INITIAL_MS * 2 ** Math.min(attempt, 8));

/** POST the bound port until the env accepts it (200) or another register already won (409). */
async function register(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      await res.arrayBuffer().catch(() => undefined);
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) throw new Error(`register answered HTTP ${res.status}`);
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered HTTP ')) throw err;
    }
    await sleep(retryDelay(attempt));
  }
}

/** Poll GET /done until {"done": true}; never return before that. */
async function waitForDone(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/done`, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      } else {
        await res.arrayBuffer().catch(() => undefined);
      }
    } catch {
      // not answering yet, or timed out: poll again
    }
    await sleep(Math.min(2_000, 250 * 2 ** Math.min(attempt, 3)));
  }
}

// A request that throws must never take the forge down with it.
process.on('uncaughtException', (err) => process.stderr.write(`[forge] uncaught: ${err.stack ?? err.message}\n`));
process.on('unhandledRejection', (err) => process.stderr.write(`[forge] unhandled: ${String(err)}\n`));

async function main(): Promise<void> {
  const server = createApp();
  const wanted = ENV_URL === '' ? Number(process.env.FORGE_PORT ?? 0) : 0;
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(wanted, () => resolve());
  });
  const address = server.address();
  if (address === null || typeof address === 'string') throw new Error('the server did not bind a TCP port');
  process.stdout.write(`[forge] listening on port ${address.port}\n`);
  if (ENV_URL === '') return; // standalone: serve until killed

  await register(address.port);
  process.stdout.write('[forge] registered; waiting for the env to say done\n');
  await waitForDone();
  server.close();
  process.stdout.write('[forge] done\n');
  process.exit(0);
}

main().catch((err: unknown) => {
  process.stderr.write(`[forge] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
});
