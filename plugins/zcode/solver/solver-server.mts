/**
 * solver-server — standalone HTTP captcha solver for the CPA ZCode plugin.
 *
 * Runs the upstream happy-dom Aliyun Captcha V3 solver (captcha-happy.ts) as an
 * independent Node process. The Go plugin calls this over loopback HTTP to get
 * a freshly-solved verify param and injects it into its in-memory pool, so the
 * start-plan auto-claimer consumes pre-solved tokens without blocking on a
 * solve in the hot path.
 *
 * Endpoints:
 *   GET  /health          -> {ok:true, ready:<n>, pool:...}
 *   POST /solve           -> {verifyParam, region, elapsedMs}
 *       body: {"scene":"...","region":"...","prefix":"...","timeoutMs":30000}
 *   GET  /stats           -> counters
 *   POST /shutdown        -> graceful exit
 *
 * Security: binds 127.0.0.1 only, no auth (loopback trust). Never logs the
 * verify param body, only length + status.
 */
import { solveTraceless } from "./captcha-happy.ts";
import http from "node:http";
import crypto from "node:crypto";

const PORT = Number(process.env.SOLVER_PORT || 8777);
const HOST = process.env.SOLVER_HOST || "127.0.0.1";

const stats = { solved: 0, failed: 0, lastError: "", startedAt: Date.now() };

function send(res, status, obj) {
  const body = JSON.stringify(obj);
  res.writeHead(status, {
    "content-type": "application/json; charset=utf-8",
    "cache-control": "no-store",
  });
  res.end(body);
}

function readBody(req, limit = 64 * 1024) {
  return new Promise((resolve, reject) => {
    let size = 0;
    const chunks = [];
    req.on("data", (c) => {
      size += c.length;
      if (size > limit) {
        reject(new Error("body too large"));
        req.destroy();
        return;
      }
      chunks.push(c);
    });
    req.on("end", () => resolve(Buffer.concat(chunks).toString("utf8")));
    req.on("error", reject);
  });
}

async function handleSolve(req, res) {
  let payload;
  try {
    const raw = await readBody(req);
    payload = JSON.parse(raw || "{}");
  } catch (e) {
    return send(res, 400, { ok: false, error: "bad request: " + e.message });
  }
  const scene = String(payload.scene || "11xygtvd");
  const region = String(payload.region || "sgp");
  const prefix = String(payload.prefix || "no8xfe");
  const timeoutMs = Number(payload.timeoutMs || 30000);
  const started = Date.now();
  try {
    const verifyParam = await solveTraceless({ scene, region, prefix, timeoutMs });
    stats.solved++;
    const elapsedMs = Date.now() - started;
    // Never log the token itself.
    console.log(`[solver] ok region=${region} len=${verifyParam.length} elapsedMs=${elapsedMs}`);
    return send(res, 200, { ok: true, verifyParam, region, elapsedMs });
  } catch (e) {
    stats.failed++;
    stats.lastError = String((e && e.message) || e).slice(0, 300);
    console.error(`[solver] fail region=${region} err=${stats.lastError}`);
    return send(res, 502, { ok: false, error: "solve failed: " + stats.lastError });
  }
}

const server = http.createServer(async (req, res) => {
  try {
    const url = new URL(req.url, "http://" + req.headers.host || "http://127.0.0.1");
    const path = url.pathname;
    if (req.method === "GET" && path === "/health") {
      return send(res, 200, {
        ok: true,
        up: Date.now() - stats.startedAt < 60_000,
        solved: stats.solved,
        failed: stats.failed,
        uptimeMs: Date.now() - stats.startedAt,
      });
    }
    if (req.method === "POST" && path === "/solve") {
      return handleSolve(req, res);
    }
    if (req.method === "GET" && path === "/stats") {
      return send(res, 200, { ok: true, stats });
    }
    if (req.method === "POST" && path === "/shutdown") {
      console.log("[solver] shutdown requested");
      send(res, 200, { ok: true });
      setTimeout(() => process.exit(0), 50);
      return;
    }
    return send(res, 404, { ok: false, error: "not found: " + path });
  } catch (e) {
    return send(res, 500, { ok: false, error: "internal: " + e.message });
  }
});

server.listen(PORT, HOST, () => {
  console.log(`[solver] listening on http://${HOST}:${PORT}`);
});
server.on("error", (e) => {
  console.error("[solver] server error:", e.message);
  process.exit(1);
});
process.on("SIGTERM", () => {
  console.log("[solver] SIGTERM");
  process.exit(0);
});
process.on("SIGINT", () => {
  console.log("[solver] SIGINT");
  process.exit(0);
});