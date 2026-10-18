// U4 acceptance harness — server lifecycle.
// Starts an isolated bin/grayling against a throwaway store so the axe audit,
// performance baseline and keyboard walkthrough all run against the real Go
// server with real storage (zero mocks).
//
// Environment knobs:
//   U4_PORT     first port to try (default 28090); busy ports probe upward.
//   U4_TMP      scratch root for the store + gonacos dump (default $TMPDIR or os.tmpdir()).
//   U4_REBUILD  '1' forces `make build` even when bin/grayling exists.
import { spawn, spawnSync, execFileSync } from 'node:child_process';
import { existsSync, mkdtempSync, rmSync, statSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

export const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..', '..', '..');

const DEFAULT_PORT = 28090;
const HEALTH_TIMEOUT_MS = 30_000;

// ensureBinary builds bin/grayling when missing (or when U4_REBUILD=1). Go
// builds on this box must not use the root partition's /tmp, so TMPDIR is
// forced to $HOME/tmp-go unless the caller already set one.
export function ensureBinary(log = console.log) {
  const bin = path.join(REPO_ROOT, 'bin', 'grayling');
  if (existsSync(bin) && process.env.U4_REBUILD !== '1') {
    log(`[server] using existing ${bin}`);
    return bin;
  }
  log('[server] building bin/grayling (make build)…');
  const env = { ...process.env };
  if (!env.TMPDIR) env.TMPDIR = path.join(env.HOME || '/home/vipas', 'tmp-go');
  const res = spawnSync('make', ['build'], { cwd: REPO_ROOT, env, stdio: 'inherit' });
  if (res.status !== 0) throw new Error('make build failed');
  if (!existsSync(bin)) throw new Error('make build completed but bin/grayling is missing');
  return bin;
}

// gonacosRoot resolves the pinned gonacos module dir exactly like `make run`.
function gonacosRoot() {
  try {
    return execFileSync('go', ['list', '-m', '-f', '{{.Dir}}', 'github.com/godeps/gonacos'], {
      cwd: REPO_ROOT,
      env: { ...process.env, GOWORK: 'off', TMPDIR: process.env.TMPDIR || path.join(process.env.HOME || '/home/vipas', 'tmp-go') },
      encoding: 'utf8',
    }).trim();
  } catch {
    return '';
  }
}

async function portFree(port) {
  const { createServer } = await import('node:net');
  return new Promise(resolve => {
    const srv = createServer();
    srv.once('error', () => resolve(false));
    srv.once('listening', () => srv.close(() => resolve(true)));
    srv.listen(port, '127.0.0.1');
  });
}

export async function pickPort(log = console.log) {
  let port = Number(process.env.U4_PORT || DEFAULT_PORT);
  for (let i = 0; i < 20; i++, port++) {
    if (await portFree(port)) return port;
  }
  throw new Error(`no free port in ${process.env.U4_PORT || DEFAULT_PORT}..+20`);
}

// startServer boots the isolated instance and resolves once /api/v1/health
// answers 200. Returns { baseURL, storeDir, stop() }.
export async function startServer({ port, log = console.log } = {}) {
  const bin = ensureBinary(log);
  port = port ?? (await pickPort(log));
  const scratch = process.env.U4_TMP || process.env.TMPDIR || tmpdir();
  const storeDir = mkdtempSync(path.join(scratch, 'grayling-u4-'));
  const storeFile = path.join(storeDir, 'store.json');
  const gonacosDir = path.join(storeDir, 'gonacos');
  const args = [
    '-addr', `127.0.0.1:${port}`,
    '-data', storeFile,
    '-gonacos-data', gonacosDir,
    '-gonacos-addr', '127.0.0.1:0',
    '-gonacos-grpc-addr', '127.0.0.1:0',
  ];
  const root = gonacosRoot();
  if (root) args.push('-gonacos-root', root);
  const env = { ...process.env, GRAYLING_DEV_AUTOLOGIN: '1' };
  log(`[server] starting ${bin} ${args.join(' ')} (store ${storeDir})`);
  const child = spawn(bin, args, { cwd: storeDir, env, stdio: ['ignore', 'pipe', 'pipe'] });
  const serverLog = [];
  child.stdout.on('data', d => serverLog.push(d.toString()));
  child.stderr.on('data', d => serverLog.push(d.toString()));

  const baseURL = `http://127.0.0.1:${port}`;
  const deadline = Date.now() + HEALTH_TIMEOUT_MS;
  let lastErr = '';
  for (;;) {
    if (child.exitCode !== null) {
      throw new Error(`server exited during boot (code ${child.exitCode})\n${serverLog.join('').slice(-4000)}`);
    }
    if (Date.now() > deadline) {
      child.kill('SIGKILL');
      throw new Error(`server did not become healthy within ${HEALTH_TIMEOUT_MS}ms\n${serverLog.join('').slice(-4000)}`);
    }
    try {
      const res = await fetch(baseURL + '/api/v1/health');
      if (res.ok) break;
      lastErr = `HTTP ${res.status}`;
    } catch (e) {
      lastErr = e.message;
    }
    await new Promise(r => setTimeout(r, 250));
  }
  log(`[server] healthy at ${baseURL}`);

  let stopped = false;
  const stop = async () => {
    if (stopped) return;
    stopped = true;
    child.kill('SIGTERM');
    const exited = await new Promise(resolve => {
      const timer = setTimeout(() => resolve(false), 5000);
      child.once('exit', () => {
        clearTimeout(timer);
        resolve(true);
      });
    });
    if (!exited) child.kill('SIGKILL');
    rmSync(storeDir, { recursive: true, force: true });
    log(`[server] stopped; removed ${storeDir}`);
  };
  process.once('SIGINT', () => {
    child.kill('SIGKILL');
    process.exit(130);
  });
  return { baseURL, port, storeDir, stop, log: serverLog };
}

// attachOrStart honours U4_BASE_URL: when set, the harness drives an already
// running instance (used by run-server.sh + manual reruns) and skips build,
// boot and teardown.
export async function attachOrStart(opts = {}) {
  const log = opts.log || console.log;
  if (process.env.U4_BASE_URL) {
    const baseURL = process.env.U4_BASE_URL.replace(/\/$/, '');
    const res = await fetch(baseURL + '/api/v1/health');
    if (!res.ok) throw new Error(`U4_BASE_URL ${baseURL} unhealthy: HTTP ${res.status}`);
    log(`[server] attaching to ${baseURL} (U4_BASE_URL)`);
    return { baseURL, port: Number(new URL(baseURL).port), storeDir: null, stop: async () => {}, attached: true };
  }
  return startServer(opts);
}
