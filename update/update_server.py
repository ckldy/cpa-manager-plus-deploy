#!/usr/bin/env python3
"""CPA 一键更新本地服务 — 仅监听 127.0.0.1，经 nginx 暴露。
鉴权：请求头 Authorization: Bearer <管理密钥>，与 config.yaml remote-management.secret-key bcrypt 比对。
可选：若 /opt/cpa-update/test.key 存在，其内容（明文）也接受（用于端到端验证，验证后删除该文件）。
端点：
  GET  /health          -> {"ok": true}
  GET  /version-status  -> 当前 vs 最新版本对比（需鉴权）
  POST /update          -> 触发更新脚本（异步 202；重复触发 409）
  GET  /update          -> 返回当前更新日志尾部（需鉴权）
"""
import json
import os
import re
import subprocess
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

CONFIG_PATH = "/root/cpa-manager-plus/cliproxyapi/config.yaml"
TEST_KEY_PATH = "/opt/cpa-update/test.key"
UPDATE_SCRIPT = "/opt/cpa-update/update.sh"
LOG_PATH = "/opt/cpa-update/update.log"
ENV_PATH = "/root/cpa-manager-plus/.env"
HOST, PORT = "127.0.0.1", 18444

_lock = threading.Lock()
_running = False


def _secret_hash():
    try:
        with open(CONFIG_PATH, encoding="utf-8") as f:
            for line in f:
                line = line.strip()
                if "secret-key:" in line:
                    v = line.split("secret-key:", 1)[1].strip().strip('"').strip("'")
                    if v.startswith("$2"):
                        return v
    except OSError:
        pass
    return ""


def _verify(header):
    if not header or not header.startswith("Bearer "):
        return False
    key = header[len("Bearer "):].strip()
    if not key:
        return False
    h = _secret_hash()
    if h:
        try:
            import bcrypt
            if bcrypt.checkpw(key.encode(), h.encode()):
                return True
        except Exception:
            pass
    try:
        with open(TEST_KEY_PATH, encoding="utf-8") as f:
            tk = f.read().strip()
        if tk and key == tk:
            return True
    except OSError:
        pass
    return False


def _github_token():
    try:
        with open(ENV_PATH, encoding="utf-8") as f:
            for line in f:
                if line.strip().startswith("GITHUB_TOKEN="):
                    return line.split("=", 1)[1].strip().strip('"').strip("'")
    except OSError:
        pass
    return ""


def _github_latest(repo):
    """返回 GitHub 仓库最新 release tag（带 token，失败返回 None）。"""
    token = _github_token()
    import urllib.request
    url = f"https://api.github.com/repos/{repo}/releases/latest"
    req = urllib.request.Request(url)
    req.add_header("Accept", "application/vnd.github+json")
    req.add_header("User-Agent", "CLIProxyAPI")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    try:
        with urllib.request.urlopen(req, timeout=15) as r:
            data = json.load(r)
            return data.get("tag_name")
    except Exception:
        return None


def _parse_version(v):
    if not v:
        return None
    m = re.search(r"(\d+)(?:\.(\d+))?(?:\.(\d+))?", v.replace("v", ""))
    if not m:
        return None
    return tuple(int(x or 0) for x in m.groups())


def _cmp_version(a, b):
    pa, pb = _parse_version(a), _parse_version(b)
    if pa is None or pb is None:
        return None
    return (pa > pb) - (pa < pb)


def _current_kernel_version():
    try:
        out = subprocess.run(
            ["docker", "logs", "cpamp-cli-proxy-api-1"], capture_output=True, text=True, timeout=30
        ).stdout
        matches = re.findall(r"CLIProxyAPI Version:\s*(v?[\d.]+)", out)
        return matches[-1] if matches else None
    except Exception:
        return None


def _current_manager_version():
    try:
        out = subprocess.run(
            ["curl", "-s", "--max-time", "20", "http://127.0.0.1:18317/management.html"],
            capture_output=True, text=True, timeout=30,
        ).stdout
        m = re.search(r"appVersion\s*:\s*[`\"']?(v?[\d.]+)", out)
        return m.group(1) if m else None
    except Exception:
        return None


def _version_status():
    kc = _current_kernel_version()
    kl = _github_latest("router-for-me/CLIProxyAPI")
    mc = _current_manager_version()
    ml = _github_latest("seakee/CPA-Manager-Plus")
    kc_has = _cmp_version(kl, kc)
    mc_has = _cmp_version(ml, mc)
    return {
        "hasUpdate": (kc_has == 1) or (mc_has == 1),
        "kernel": {"current": kc, "latest": kl, "hasUpdate": kc_has == 1},
        "manager": {"current": mc, "latest": ml, "hasUpdate": mc_has == 1},
    }


def _run_update():
    global _running
    with _lock:
        if _running:
            return
        _running = True
    try:
        subprocess.run(["bash", UPDATE_SCRIPT], timeout=600)
    finally:
        with _lock:
            _running = False


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        pass

    def _send(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path.startswith("/health"):
            self._send(200, {"ok": True})
            return
        if self.path.startswith("/version-status"):
            if not _verify(self.headers.get("Authorization", "")):
                self._send(401, {"error": "invalid management key"})
                return
            self._send(200, _version_status())
            return
        if self.path.startswith("/update"):
            if not _verify(self.headers.get("Authorization", "")):
                self._send(401, {"error": "invalid management key"})
                return
            try:
                with open(LOG_PATH, encoding="utf-8", errors="replace") as f:
                    log = f.read()
            except OSError:
                log = ""
            self._send(200, {"ok": True, "running": _running, "log": log[-8000:]})
            return
        self._send(404, {"error": "not found"})

    def do_POST(self):
        if self.path.startswith("/update"):
            if not _verify(self.headers.get("Authorization", "")):
                self._send(401, {"error": "invalid management key"})
                return
            with _lock:
                if _running:
                    self._send(409, {"error": "update already running"})
                    return
            threading.Thread(target=_run_update, daemon=True).start()
            self._send(202, {"ok": True, "message": "update started"})
            return
        self._send(404, {"error": "not found"})


if __name__ == "__main__":
    print(f"CPA update service on {HOST}:{PORT}", flush=True)
    ThreadingHTTPServer((HOST, PORT), Handler).serve_forever()
