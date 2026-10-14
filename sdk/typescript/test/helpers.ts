/**
 * Integration harness: builds and runs the REAL grayling binary (GOWORK=off)
 * and bootstraps a workspace/environment with tokens and published flags —
 * the same startup contract as test/boot (owner token scraped from stderr,
 * free ports, health poll). Nothing is mocked.
 */

import { spawn, spawnSync, type ChildProcessByStdio } from "node:child_process";
import { once } from "node:events";
import { mkdtemp, rm } from "node:fs/promises";
import { createInterface } from "node:readline";
import type { Readable } from "node:stream";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import net from "node:net";

type GraylingProc = ChildProcessByStdio<null, Readable, Readable>;

const HERE = dirname(fileURLToPath(import.meta.url));
export const REPO_ROOT = join(HERE, "..", "..", "..");

async function freePort(): Promise<number> {
  const srv = net.createServer();
  await new Promise<void>((res) => srv.listen(0, "127.0.0.1", res));
  const port = (srv.address() as net.AddressInfo).port;
  await new Promise<void>((res) => srv.close(() => res()));
  return port;
}

function gonacosRoot(): string {
  const out = spawnSync(
    "go", ["list", "-m", "-f", "{{.Dir}}", "github.com/godeps/gonacos"],
    { cwd: REPO_ROOT, env: { ...process.env, GOWORK: "off" }, encoding: "utf8" });
  if (out.status !== 0) throw new Error(`go list gonacos: ${out.stderr}`);
  return out.stdout.trim();
}

export interface AdminResponse {
  status: number;
  body: any;
}

export class GraylingServer {
  readonly baseUrl: string;
  readonly ownerToken: string;
  reviewerToken = "";
  readonly proc: GraylingProc;
  readonly tmp: string;

  constructor(baseUrl: string, ownerToken: string,
    proc: GraylingProc, tmp: string) {
    this.baseUrl = baseUrl;
    this.ownerToken = ownerToken;
    this.proc = proc;
    this.tmp = tmp;
  }

  async request(method: string, path: string, token?: string, body?: unknown): Promise<AdminResponse> {
    const headers: Record<string, string> = { Accept: "application/json" };
    if (token) headers.Authorization = `Bearer ${token}`;
    let payload: string | undefined;
    if (body !== undefined) {
      payload = JSON.stringify(body);
      headers["Content-Type"] = "application/json";
    }
    const resp = await fetch(this.baseUrl + path, { method, headers, body: payload });
    const text = await resp.text();
    return { status: resp.status, body: text ? JSON.parse(text) : null };
  }

  async expect(method: string, path: string, status: number, token?: string, body?: unknown): Promise<any> {
    const res = await this.request(method, path, token, body);
    if (res.status !== status) {
      throw new Error(`${method} ${path} → ${res.status}, want ${status}: ${JSON.stringify(res.body).slice(0, 400)}`);
    }
    return res.body;
  }

  async publishFlag(ws: string, env: string, content: Record<string, unknown>,
    opts: { approver?: string; risk?: string } = {}): Promise<any> {
    const base = `/api/v1/admin/workspaces/${ws}/environments/${env}/flags/${content.key}`;
    await this.expect("PUT", `${base}/draft`, 200, this.ownerToken,
      { content, risk: opts.risk ?? "normal" });
    await this.expect("POST", `${base}/submit`, 200, this.ownerToken, { reason: "TypeScript SDK integration" });
    await this.expect("POST", `${base}/approve`, 200, opts.approver ?? this.reviewerToken, { note: "ok" });
    return await this.expect("POST", `${base}/publish`, 200, this.ownerToken, { note: "ship" });
  }

  async stop(): Promise<void> {
    this.proc.kill("SIGTERM");
    await Promise.race([once(this.proc, "exit"), new Promise((r) => setTimeout(r, 15_000))]);
    if (this.proc.exitCode === null) this.proc.kill("SIGKILL");
    await rm(this.tmp, { recursive: true, force: true });
  }
}

export async function startServer(tag: string): Promise<GraylingServer> {
  const tmp = await mkdtemp(join(tmpdir(), `grayling-tssdk-${tag}-`));
  const binary = join(tmp, "grayling");
  const build = spawnSync("go", ["build", "-o", binary, "./cmd/grayling"],
    { cwd: REPO_ROOT, env: { ...process.env, GOWORK: "off" }, encoding: "utf8" });
  if (build.status !== 0) throw new Error(`go build failed: ${build.stderr}`);

  const [apiPort, napPort, grpcPort] = [await freePort(), await freePort(), await freePort()];
  const proc = spawn(binary, [
    "-addr", `127.0.0.1:${apiPort}`,
    "-data", join(tmp, "store.json"),
    "-gonacos-root", gonacosRoot(),
    "-gonacos-data", join(tmp, "gonacos"),
    "-gonacos-addr", `127.0.0.1:${napPort}`,
    "-gonacos-grpc-addr", `127.0.0.1:${grpcPort}`,
  ], { stdio: ["ignore", "pipe", "pipe"] }) as GraylingProc;

  // Scrape the one-time bootstrap owner token from stderr.
  const tokenPromise = new Promise<string>((resolve, reject) => {
    const rl = createInterface({ input: proc.stderr });
    const timer = setTimeout(() => reject(new Error("owner token not printed")), 30_000);
    rl.on("line", (line) => {
      const idx = line.indexOf("grayling_admin_");
      if (idx >= 0) {
        clearTimeout(timer);
        resolve(line.slice(idx).split(/\s+/)[0].trim());
        rl.close();
      }
    });
    proc.on("exit", (code) => {
      clearTimeout(timer);
      reject(new Error(`grayling exited early: ${code}`));
    });
  });
  const ownerToken = await tokenPromise;

  const baseUrl = `http://127.0.0.1:${apiPort}`;
  const deadline = Date.now() + 30_000;
  for (;;) {
    try {
      const r = await fetch(`${baseUrl}/api/v1/health`);
      if (r.ok) break;
    } catch { /* not up yet */ }
    if (Date.now() > deadline) {
      proc.kill("SIGKILL");
      throw new Error("server never became healthy");
    }
    await new Promise((res) => setTimeout(res, 200));
  }
  return new GraylingServer(baseUrl, ownerToken, proc, tmp);
}

/** A prepared workspace/environment with tokens and published flags. */
export class TestEnv {
  readonly ws = "tsws";
  readonly env = "prod";
  readonly server: GraylingServer;
  sdkToken = "";
  collectToken = "";
  publisherToken = "";
  experimentId = "";
  epoch = 0;

  constructor(server: GraylingServer) {
    this.server = server;
  }

  static async create(server: GraylingServer): Promise<TestEnv> {
    const t = new TestEnv(server);
    await server.expect("POST", "/api/v1/admin/workspaces", 201, server.ownerToken,
      { slug: t.ws, name: t.ws });
    await server.expect("POST", `/api/v1/admin/workspaces/${t.ws}/environments`, 201,
      server.ownerToken, { slug: t.env, name: t.env });
    t.sdkToken = (await server.expect("POST", `/api/v1/admin/workspaces/${t.ws}/tokens`,
      201, server.ownerToken, { name: "ts-sdk", kind: "sdk", environment: t.env })).Secret;
    t.collectToken = (await server.expect("POST", `/api/v1/admin/workspaces/${t.ws}/tokens`,
      201, server.ownerToken, { name: "ts-collect", kind: "collect", environment: t.env })).Secret;
    t.publisherToken = (await server.expect("POST", `/api/v1/admin/workspaces/${t.ws}/tokens`,
      201, server.ownerToken, { name: "ts-publisher", kind: "admin", role: "publisher" })).Secret;
    server.reviewerToken = t.publisherToken;

    // Bool flag: tenant=acme → on; default off.
    await server.publishFlag(t.ws, t.env, {
      schemaVersion: 1, key: "ts-bool", type: "bool", fields: ["tenant"],
      variations: { on: true, off: false },
      defaultRule: { variation: "off" },
      targeting: [{
        name: "acme",
        constraints: [{ field: "tenant", op: "eq", value: "acme" }],
        variation: "on",
      }],
    });
    await server.publishFlag(t.ws, t.env, {
      schemaVersion: 1, key: "ts-string", type: "string",
      variations: { a: "alpha", b: "beta" },
      defaultRule: { variation: "a" },
    });
    await server.publishFlag(t.ws, t.env, {
      schemaVersion: 1, key: "ts-number", type: "number",
      variations: { low: 1, high: 42.5 },
      defaultRule: { variation: "high" },
    });
    await server.publishFlag(t.ws, t.env, {
      schemaVersion: 1, key: "ts-json", type: "json",
      variations: { cfg: { model: "qwen-max", temperature: 0.2 } },
      defaultRule: { variation: "cfg" },
    });
    // Experiment flag: 50/50 split, registered + bound + running.
    await server.publishFlag(t.ws, t.env, {
      schemaVersion: 1, key: "ts-exp", type: "string", fields: ["user_id"],
      bucketing: { algorithm: "grayling-fnv1a-10000-v1", unit: "user_id", salt: "ts-exp-salt" },
      variations: { control: "c", treatment: "t" },
      defaultRule: { variation: "control" },
      targeting: [{ name: "split", percentage: { control: 50, treatment: 50 } }],
    });
    const exp = await server.expect("POST",
      `/api/v1/admin/workspaces/${t.ws}/environments/${t.env}/experiments`, 201,
      server.ownerToken, {
        flagKey: "ts-exp", unit: "user_id",
        variants: ["control", "treatment"],
        proportions: { control: 50, treatment: 50 },
        primaryMetric: { key: "conversion", type: "proportion", direction: "increase" },
        alpha: 0.05, power: 0.8, mde: 0.02,
        minSamplePerArm: 5, minRuntime: 0,
        analysisWindow: 7 * 24 * 3600 * 1e9,
        hypothesis: "treatment lifts conversion",
      });
    t.experimentId = exp.id;
    t.epoch = exp.epoch;
    await server.expect("POST", `/api/v1/admin/experiments/${t.experimentId}/review`, 200,
      server.ownerToken, { note: "pre-registered" });
    await server.expect("POST", `/api/v1/admin/experiments/${t.experimentId}/bind`, 200,
      server.ownerToken);
    // High-risk binding draft: second actor approves (二人规则).
    await server.expect("POST",
      `/api/v1/admin/workspaces/${t.ws}/environments/${t.env}/flags/ts-exp/approve`, 200,
      t.publisherToken, { note: "second pair of eyes" });
    await server.expect("POST",
      `/api/v1/admin/workspaces/${t.ws}/environments/${t.env}/flags/ts-exp/publish`, 200,
      server.ownerToken, { note: "bind" });
    await server.expect("POST", `/api/v1/admin/experiments/${t.experimentId}/transition`, 200,
      server.ownerToken, { to: "running", reason: "launch" });
    return t;
  }

  async report(): Promise<any> {
    return await this.server.expect("GET",
      `/api/v1/admin/experiments/${this.experimentId}/report`, 200, this.server.ownerToken);
  }

  async republishBool(defaultVariation: string): Promise<any> {
    return await this.server.publishFlag(this.ws, this.env, {
      schemaVersion: 1, key: "ts-bool", type: "bool", fields: ["tenant"],
      variations: { on: true, off: false },
      defaultRule: { variation: defaultVariation },
      targeting: [{
        name: "acme",
        constraints: [{ field: "tenant", op: "eq", value: "acme" }],
        variation: "on",
      }],
    });
  }
}
