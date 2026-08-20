"""
Windows Compute Agent — 通用按需计算节点
服务端通过 HTTP 调用，Windows 开机即用，关机不影响服务器。
端口和环境变量 COMPUTE_PORT 可配置。
认证：COMPUTE_TOKEN 必须配置（fail-closed），/cmd 与 /python 需 Bearer token。
"""
import hmac
import json, subprocess, sys, os, time, traceback, tempfile
from http.server import HTTPServer, BaseHTTPRequestHandler

PORT = int(os.environ.get("COMPUTE_PORT", "19527"))
ALLOWED_IPS = {"127.0.0.1", "::1"}  # localhost, CIDR covers servers below
ALLOWED_CIDRS = [
    # Default allowlist is EMPTY for open-source deployments — configure
    # COMPUTE_ALLOWED_CIDRS with your own server's Tailscale /32.
    # blanket covered the entire CGNAT carrier range — same-ISP neighbors in
    # the /10 could reach the agent (R3). The list starts empty; extend via
    # COMPUTE_ALLOWED_CIDRS env when adding callers.
]
# Optional extra CIDRs via env, e.g. COMPUTE_ALLOWED_CIDRS=10.1.2.0/24,192.168.0.0/16
for part in os.environ.get("COMPUTE_ALLOWED_CIDRS", "").split(","):
    part = part.strip()
    if "/" not in part:
        continue
    try:
        ip, bits = part.split("/", 1)
        bits = int(bits)
        if not 0 < bits <= 32:
            continue
        p = ip.split(".")
        mask = (0xFFFFFFFF << (32 - bits)) & 0xFFFFFFFF
        ip_int = (int(p[0]) << 24) | (int(p[1]) << 16) | (int(p[2]) << 8) | int(p[3])
        ALLOWED_CIDRS.append((ip_int, mask))
    except Exception:
        print(f"ignoring invalid COMPUTE_ALLOWED_CIDRS entry: {part!r}")

COMPUTE_TOKEN = os.environ.get("COMPUTE_TOKEN", "")


def token_ok(handler):
    """Fail-closed token check. No COMPUTE_TOKEN configured = no exec access."""
    if not COMPUTE_TOKEN:
        return False
    auth = handler.headers.get("Authorization", "")
    return hmac.compare_digest(auth.encode("utf-8"), ("Bearer " + COMPUTE_TOKEN).encode("utf-8"))


def ip_allowed(addr):
    """Check if IP is in allowlist or configured CIDRs."""
    if addr in ALLOWED_IPS:
        return True
    # IPv4-mapped IPv6 source addresses (::ffff:1.2.3.4) arrive from
    # dual-stack sockets — normalize before the octet parse (R3: the old
    # parse crashed with ValueError on them).
    if addr.startswith("::ffff:"):
        addr = addr[7:]
    parts = addr.split(".")
    if len(parts) != 4:
        return False
    try:
        ip_int = (int(parts[0]) << 24) | (int(parts[1]) << 16) | (int(parts[2]) << 8) | int(parts[3])
    except ValueError:
        return False
    for net, mask in ALLOWED_CIDRS:
        if (ip_int & mask) == (net & mask):
            return True
    return False


def _run_to_files(cmd, shell, timeout, cwd, env):
    """Run subprocess with stdout/stderr written to temp files (NOT memory).
    Returns (returncode, stdout_tail, stderr_tail). API contract unchanged."""
    so = tempfile.NamedTemporaryFile(mode="wb", delete=False)
    se = tempfile.NamedTemporaryFile(mode="wb", delete=False)
    try:
        r = subprocess.run(cmd, shell=shell, stdout=so, stderr=se,
                           timeout=timeout, cwd=cwd, env=env)

        def tail(path, n):
            with open(path, "rb") as f:
                f.seek(0, 2)
                size = f.tell()
                f.seek(max(0, size - n * 4))
                return f.read().decode("utf-8", errors="replace")[-n:]

        return r.returncode, tail(so.name, 8000), tail(se.name, 8000)
    finally:
        so.close()
        se.close()
        os.unlink(so.name)
        os.unlink(se.name)


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print(f"[{time.strftime('%H:%M:%S')}] {args[0]}")

    def _send_json(self, data, status=200):
        body = json.dumps(data, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", len(body))
        self.end_headers()
        self.wfile.write(body)

    def _check_ip(self):
        client_ip = self.client_address[0]
        if not ip_allowed(client_ip):
            self._send_json({"error": f"denied: {client_ip}"}, 403)
            return False
        return True

    def do_GET(self):
        if not self._check_ip():
            return
        if self.path == "/health":
            self._send_json({
                "hostname": os.environ.get("COMPUTERNAME", ""),
                "status": "ok",
                "ts": time.time(),
            })
        else:
            self._send_json({"error": "not found"}, 404)

    def do_POST(self):
        if not self._check_ip():
            return
        if self.path not in ("/cmd", "/python"):
            self._send_json({"error": "not found"}, 404)
            return
        if not token_ok(self):
            self._send_json({"error": "token required"}, 403)
            return
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length)) if length else {}
        if self.path == "/cmd":
            self._handle_cmd(body)
        else:
            self._handle_python(body)

    def _handle_cmd(self, body):
        cmd = body.get("cmd", "")
        timeout = body.get("timeout", 60)
        cwd = body.get("cwd", None)
        if not cmd:
            self._send_json({"error": "missing cmd"}, 400)
            return
        t0 = time.time()
        try:
            code, stdout, stderr = _run_to_files(cmd, True, timeout, cwd, os.environ)
            self._send_json({
                "ok": code == 0,
                "code": code,
                "stdout": stdout,
                "stderr": stderr,
                "elapsed": round(time.time() - t0, 3),
            })
        except subprocess.TimeoutExpired:
            self._send_json({"ok": False, "error": "timeout", "elapsed": timeout}, 408)
        except Exception:
            traceback.print_exc()
            self._send_json({"ok": False, "error": "internal error", "elapsed": round(time.time() - t0, 3)}, 500)

    def _handle_python(self, body):
        code = body.get("code", "")
        timeout = body.get("timeout", 30)
        if not code:
            self._send_json({"error": "missing code"}, 400)
            return
        t0 = time.time()
        try:
            code, stdout, stderr = _run_to_files([sys.executable, "-c", code], False, timeout, None, None)
            self._send_json({
                "ok": code == 0,
                "code": code,
                "stdout": stdout,
                "stderr": stderr,
                "elapsed": round(time.time() - t0, 3),
            })
        except subprocess.TimeoutExpired:
            self._send_json({"ok": False, "error": "timeout", "elapsed": timeout}, 408)
        except Exception:
            traceback.print_exc()
            self._send_json({"ok": False, "error": "internal error", "elapsed": round(time.time() - t0, 3)}, 500)


def main():
    print(f"Compute Agent → 0.0.0.0:{PORT}")
    print(f"Allowed: localhost + COMPUTE_ALLOWED_CIDRS")
    print(f"Auth: {'COMPUTE_TOKEN set' if COMPUTE_TOKEN else 'WARNING — COMPUTE_TOKEN not set, /cmd /python disabled'}")
    srv = HTTPServer(("0.0.0.0", PORT), Handler)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        print("\nStopped.")


if __name__ == "__main__":
    main()
