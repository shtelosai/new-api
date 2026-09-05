#!/usr/bin/env python3
"""仅使用临时 SQLite 与本机合成上游，验证真实 TokenAuth、隔离、重试和计费日志。"""
import argparse
import concurrent.futures
import contextlib
import http.server
import hashlib
import platform
import json
import os
from pathlib import Path
import socket
import shutil
import sqlite3
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request

MODEL = "route-smoke-model"


def port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def request(url, body=None, headers=None):
    data = None if body is None else (body.encode() if isinstance(body, str) else json.dumps(body).encode())
    req = urllib.request.Request(url, data=data, headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=20) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as error:
        return error.code, error.read()


def wait_until(check, seconds=20):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            if check():
                return
        except (OSError, sqlite3.Error):
            pass
        time.sleep(0.05)
    raise AssertionError("等待本地验收状态超时")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    binary = args.binary.resolve()
    args.output.mkdir(parents=True, exist_ok=True)
    calls = []
    lock = threading.Lock()

    class Upstream(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
                chunks = []
                while True:
                    size = int(self.rfile.readline().split(b";", 1)[0], 16)
                    if size == 0:
                        while self.rfile.readline().strip():
                            pass
                        break
                    chunks.append(self.rfile.read(size))
                    assert self.rfile.read(2) == b"\r\n"
                raw = b"".join(chunks)
            else:
                raw = self.rfile.read(int(self.headers["Content-Length"]))
            body = json.loads(raw)
            tag = body.get("input", "")
            route = self.path.split("/")[1]
            with lock:
                calls.append({"tag": tag, "route": route, "path": self.path, "model": body.get("model")})
            status = 503 if tag == "fail-only-once" else 200
            if status != 200:
                payload = {"error": {"message": "synthetic unavailable", "type": "server_error", "code": "server_error"}}
            else:
                payload = {"id": "resp_synthetic", "object": "response", "created_at": 1,
                           "status": "completed", "model": MODEL,
                           "output": [{"type": "message", "role": "assistant", "status": "completed",
                                       "content": [{"type": "output_text", "text": route, "annotations": []}]}],
                           "usage": {"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}}
                if self.path.endswith("/compact"):
                    payload["object"] = "response.compaction"
                    payload["output"] = [{"type": "compaction", "encrypted_content": "synthetic"}]
            content = json.dumps(payload).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(content)))
            self.end_headers()
            self.wfile.write(content)

    upstream = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Upstream)
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    process = None
    logfile = None
    status_versions = []
    directory = None
    try:
        with contextlib.nullcontext(tempfile.mkdtemp(prefix="twork-route-smoke-")) as directory:
            root = Path(directory)
            dbpath = root / "smoke.db"
            gateway_port = port()
            base = f"http://127.0.0.1:{gateway_port}"
            env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "HOME": str(root), "TMPDIR": str(root),
                   "PORT": str(gateway_port), "SQLITE_PATH": str(dbpath), "SQL_DSN": "", "LOG_SQL_DSN": "",
                   "REDIS_CONN_STRING": "", "MEMORY_CACHE_ENABLED": "true", "SYNC_FREQUENCY": "1",
                   "BATCH_UPDATE_ENABLED": "false", "GLOBAL_API_RATE_LIMIT_ENABLE": "false",
                   "SESSION_SECRET": "local-synthetic-model-route-only", "GIN_MODE": "release"}
            logfile = (args.output / "gateway-process.log").open("w")

            def start():
                nonlocal process
                process = subprocess.Popen([str(binary)], cwd=root, env=env, stdout=logfile, stderr=subprocess.STDOUT)
                def healthy():
                    status, raw = request(base + "/api/status")
                    if status != 200:
                        return False
                    status_versions.append(json.loads(raw)["data"].get("version"))
                    return True
                wait_until(healthy, 45)

            def stop():
                nonlocal process
                if process is not None:
                    process.terminate()
                    try:
                        process.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait()
                    process = None

            start()
            stop()
            with sqlite3.connect(dbpath) as db:
                for uid, role in [(101, 1), (102, 100)]:
                    db.execute('INSERT INTO users (id,username,password,display_name,role,status,quota,"group",aff_code,setting) VALUES (?,?,?,?,?,?,?,?,?,?)',
                               (uid, f"routeuser{uid}", "local-unused", f"routeuser{uid}", role, 1, 1000000, "default", f"route{uid}", "{}"))
                for tid, uid in [(201, 101), (202, 102), (203, 102)]:
                    db.execute('INSERT INTO tokens (id,user_id,key,status,name,expired_time,remain_quota,unlimited_quota,model_limits_enabled,model_limits,"group") VALUES (?,?,?,?,?,?,?,?,?,?,?)',
                               (tid, uid, f"synthetictoken{tid}", 1, f"route{tid}", -1, 1000000, 0, 0, "", "default"))
                for cid, route, priority in [(301, "legacy", 0), (302, "codex", 100)]:
                    settings = {"pass_through_body_enabled": True}
                    if route == "codex":
                        settings["twork_runtime"] = "codex"
                    db.execute('INSERT INTO channels (id,type,key,status,name,models,"group",base_url,priority,weight,auto_ban,ratio,setting,settings,channel_info) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)',
                               (cid, 1, "syntheticupstream", 1, route, MODEL + "," + MODEL + "-openai-compact", "default",
                                f"http://127.0.0.1:{upstream.server_port}/{route}", priority, 30, 0, 1, json.dumps(settings), "{}", sqlite3.Binary(b"{}")))
                    for name in [MODEL, MODEL + "-openai-compact"]:
                        db.execute('INSERT INTO abilities ("group",model,channel_id,enabled,priority,weight) VALUES (?,?,?,?,?,?)',
                                   ("default", name, cid, 1, priority, 30))
                for tid in [201, 202]:
                    for cid in [301, 302]:
                        db.execute('INSERT INTO token_model_channels (token_id,model_id,channel_id) VALUES (?,?,?)', (tid, MODEL, cid))
                for key, value in {"ModelRatio": {MODEL: 1, MODEL + "-openai-compact": 1},
                                   "CompletionRatio": {MODEL: 1, MODEL + "-openai-compact": 1}, "GroupRatio": {"default": 1}}.items():
                    db.execute('INSERT OR REPLACE INTO options (key,value) VALUES (?,?)', (key, json.dumps(value)))
                db.execute('INSERT OR REPLACE INTO options (key,value) VALUES (?,?)', ("RetryTimes", "3"))
            start()

            def relay(tid, tag, explicit=False, path="/v1/responses", suffix="", raw=None):
                headers = {"Authorization": f"Bearer sk-synthetictoken{tid}" + suffix, "Content-Type": "application/json"}
                if explicit:
                    headers.update({"X-Twork-Channel-Id": "302", "X-Twork-Client-Version": "4.0.0",
                                    "X-Twork-Client-Capabilities": "model-routes-v1"})
                return request(base + path, raw if raw is not None else {"model": MODEL, "input": tag}, headers)

            with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
                jobs = [(201, "ordinary-old", False), (201, "ordinary-new", True), (202, "admin-old", False), (202, "admin-new", True)]
                results = list(pool.map(lambda job: relay(*job), jobs))
            for job, (status, body) in zip(jobs, results):
                assert status == 200, (job, status, body.decode())
            for _, tag, explicit in jobs:
                matches = [row for row in calls if row["tag"] == tag]
                assert len(matches) == 1 and matches[0]["route"] == ("codex" if explicit else "legacy"), (tag, matches)

            for status, body in [relay(203, "ungranted-admin", True), relay(202, "admin-suffix", suffix="-302")]:
                assert status == 403, (status, body.decode())
            assert not [row for row in calls if row["tag"] in ["ungranted-admin", "admin-suffix"]]
            status, body = relay(201, "fail-only-once", True)
            assert status == 503, (status, body.decode())
            assert [row["route"] for row in calls if row["tag"] == "fail-only-once"] == ["codex"]
            status, body = relay(201, "compact-billing", True, "/v1/responses/compact")
            assert status == 200, (status, body.decode())
            assert [row["route"] for row in calls if row["tag"] == "compact-billing"] == ["codex"]
            before = len(calls)
            for raw in ['{"model":"' + MODEL + '","model":"forbidden","input":"ambiguous"}',
                        '{"model":"' + MODEL + '","Model":"forbidden","input":"ambiguous"}']:
                status, body = relay(201, "ambiguous", True, raw=raw)
                assert status == 400, (status, body.decode())
            assert len(calls) == before

            def read_logs():
                with sqlite3.connect(dbpath) as db:
                    return db.execute('SELECT type,channel_id,model_name,quota,prompt_tokens,completion_tokens FROM logs WHERE token_id IN (201,202,203) ORDER BY id').fetchall()

            wait_until(lambda: len([row for row in read_logs() if row[0] == 2]) == 5)
            logs = read_logs()
            consumed = [row for row in logs if row[0] == 2]
            assert sum(row[1] == 301 for row in consumed) == 2, consumed
            assert sum(row[1] == 302 for row in consumed) == 3, consumed
            compact = [row for row in consumed if row[2] == MODEL + "-openai-compact"]
            assert len(compact) == 1 and compact[0][1] == 302 and compact[0][3] > 0, compact
            assert all(row[3] > 0 and row[4:] == (10, 5) for row in consumed), consumed
            # 授权缓存尚未刷新也必须直接读取已撤销的持久化授权。
            with sqlite3.connect(dbpath) as db:
                db.execute('DELETE FROM token_model_channels WHERE token_id=201 AND channel_id=302')
            status, body = relay(201, "revoked", True)
            assert status == 403, (status, body.decode())
            assert not [row for row in calls if row["tag"] == "revoked"]
            stop()
            result = {"result": "PASS", "real_token_auth": True, "synthetic_upstream_only": True,
                      "runtime_system": platform.system(), "runtime_architecture": platform.machine(),
                      "binary_sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
                      "startup_health_status_versions": status_versions,
                      "concurrent_old_new_ordinary_admin": True, "upstream_failure_attempts": 1, "configured_retry_times": 3,
                      "consume_logs": consumed, "upstream_calls": calls,
                      "compact_billing_channel": 302, "revocation_immediate": True}
            (args.output / "result.json").write_text(json.dumps(result, ensure_ascii=False, indent=2))
            print(json.dumps({"result": "PASS", "consume_logs": len(consumed), "upstream_calls": len(calls), "output": str(args.output)}, ensure_ascii=False))
    finally:
        if process is not None:
            process.terminate()
            with contextlib.suppress(subprocess.TimeoutExpired):
                process.wait(timeout=10)
            if process.poll() is None:
                process.kill()
                process.wait()
        if logfile is not None:
            logfile.close()
        upstream.shutdown()
        upstream.server_close()
        if directory is not None:
            shutil.rmtree(directory)


if __name__ == "__main__":
    main()
