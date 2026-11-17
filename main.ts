// The entry's life: start the Go core, tell the env which port it bound, serve until the env
// says the trial is over, then exit 0. The env owns port 8080, so the core never binds it; the
// kernel picks a free port with listen(0) and the env dials the address the register request came
// from. Everything the suite scores lives in the statically linked Go binary committed beside this
// file (forge-core, built from the Go sources in this repository); this file only starts it, keeps
// the process up for exactly as long as the trial runs, and shuts it down cleanly.
//
// The core is started from a writable copy when the committed file is not directly executable
// (the repository checkout may be read-only or lack the executable bit): the binary is copied to
// the system temp directory, chmodded, and spawned from there. When the committed file already
// runs, it is spawned in place.

import { spawn, type ChildProcess } from 'node:child_process';
import { accessSync, chmodSync, constants as fsConstants, copyFileSync, unlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (ENV_URL === '') {
  process.stderr.write('[forge] MAKEFUN_ENV_URL is not set: no env to register with\n');
  process.exit(1);
}

// One register or done request may take this long before it is abandoned and retried.
const REQUEST_TIMEOUT_MS = 30_000;
// Backoff between retries: 250ms, doubling, capped at 2s.
const BACKOFF_FIRST_MS = 250;
const BACKOFF_MAX_MS = 2_000;
// How long to wait for the core to print its bound port before trying the next start method.
const CORE_BOOT_TIMEOUT_MS = 15_000;
// How long to wait for the core to exit after SIGTERM before escalating to SIGKILL.
const CORE_STOP_TIMEOUT_MS = 5_000;

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

function backoff(attempt: number): number {
  return Math.min(BACKOFF_MAX_MS, BACKOFF_FIRST_MS * 2 ** Math.min(attempt, 8));
}

const CORE_SOURCE = join(dirname(fileURLToPath(import.meta.url)), 'forge-core');

/** A started core: the child process, its bound port, and a cleanup for a temp copy. */
interface StartedCore {
  readonly child: ChildProcess;
  readonly port: number;
  readonly cleanup: () => void;
}

/**
 * Spawn the core from `binary` and wait for its `listening on port N` line. Rejects when the
 * child errors, exits, or stays silent past the boot timeout; the caller tries the next method.
 */
function spawnAndReadPort(binary: string, cleanup: () => void): Promise<StartedCore> {
  return new Promise<StartedCore>((resolve, reject) => {
    const child = spawn(binary, [], { stdio: ['ignore', 'pipe', 'pipe'] });
    let settled = false;
    let buffered = '';
    const timer = setTimeout(() => {
      if (settled) return;
      settled = true;
      child.kill('SIGKILL');
      cleanup();
      reject(new Error(`the core at ${binary} printed no port within ${CORE_BOOT_TIMEOUT_MS} ms`));
    }, CORE_BOOT_TIMEOUT_MS);
    const fail = (err: Error): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      cleanup();
      reject(err);
    };
    child.once('error', fail);
    child.once('exit', (code, signal) => {
      fail(new Error(`the core at ${binary} exited before printing its port (code=${code} signal=${signal})`));
    });
    child.stdout?.on('data', (chunk: Buffer) => {
      if (settled) return;
      buffered += chunk.toString('utf8');
      const match = /listening on port (\d+)/.exec(buffered);
      if (match === null) {
        if (buffered.length > 4096) fail(new Error(`the core at ${binary} printed no port line`));
        return;
      }
      settled = true;
      clearTimeout(timer);
      // The core prints nothing else; drain anything further so it can never block on the pipe.
      child.stdout?.on('data', () => undefined);
      child.stderr?.on('data', (data: Buffer) => process.stderr.write(data));
      resolve({ child, port: Number(match[1]), cleanup });
    });
    child.stderr?.on('data', (data: Buffer) => process.stderr.write(data));
  });
}

/**
 * Start the core: in place when the committed binary is executable, else from a writable temp
 * copy. The temp copy is removed when the child exits.
 */
async function startCore(): Promise<StartedCore> {
  const errors: string[] = [];
  try {
    accessSync(CORE_SOURCE, fsConstants.X_OK);
    return await spawnAndReadPort(CORE_SOURCE, () => undefined);
  } catch (err) {
    errors.push(err instanceof Error ? err.message : String(err));
  }
  const copyPath = join(tmpdir(), `forge-core-${process.pid}`);
  try {
    copyFileSync(CORE_SOURCE, copyPath);
    chmodSync(copyPath, 0o755);
    return await spawnAndReadPort(copyPath, () => {
      try {
        unlinkSync(copyPath);
      } catch {
        // The copy is in temp storage; leaving it behind is harmless.
      }
    });
  } catch (err) {
    errors.push(err instanceof Error ? err.message : String(err));
  }
  throw new Error(`could not start the Go core: ${errors.join('; ')}`);
}

/**
 * POST the bound port until the env accepts it. 200 is normal; 409 means a first register already
 * won and this process waits like any other. 400/405 are contract errors and end the program;
 * anything else (a network error, another status, a timeout) is retried, since the env may still
 * be starting.
 */
async function postPort(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) {
        throw new Error(`register refused: HTTP ${res.status}: ${await res.text()}`);
      }
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register refused:')) throw err;
    }
    await sleep(backoff(attempt));
  }
}

/** Poll GET /done until it answers {"done": true}; never return before that. */
async function awaitDone(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/done`, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // The env is not answering yet, or the poll timed out: not done, poll again.
    }
    await sleep(backoff(attempt));
  }
}

async function main(): Promise<void> {
  const core = await startCore();
  let shuttingDown = false;
  // If the core dies mid-trial, fail loudly instead of polling /done for a dead entry.
  core.child.once('exit', () => {
    if (!shuttingDown) {
      process.stderr.write('[forge] the Go core exited unexpectedly\n');
      process.exit(1);
    }
  });
  const stopCore = async (): Promise<void> => {
    if (shuttingDown) return;
    shuttingDown = true;
    core.child.kill('SIGTERM');
    await Promise.race([
      new Promise<void>((resolve) => core.child.once('exit', () => resolve())),
      sleep(CORE_STOP_TIMEOUT_MS).then(() => {
        core.child.kill('SIGKILL');
      }),
    ]);
    core.cleanup();
  };
  process.once('SIGTERM', () => void stopCore().then(() => process.exit(0)));
  process.once('SIGINT', () => void stopCore().then(() => process.exit(0)));

  process.stdout.write(`[forge] listening on port ${core.port}\n`);

  await postPort(core.port);
  process.stdout.write('[forge] registered; serving until the env says done\n');

  await awaitDone();
  await stopCore();
  process.stdout.write('[forge] done\n');
}

try {
  await main();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[forge] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
