#!/usr/bin/env python3
"""使用真实网关、临时 SQLite 和本机假上游验证企业签名、计费及精确渠道。"""

import argparse
import hashlib
import hmac
import http.server
import json
import os
import sqlite3
import subprocess
import tempfile
import threading
import time
from pathlib import Path

from verify_twork_model_routes import port, request, wait_until


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)
    key = "employee-chat-test-signing-key-32bytes"
    calls = []

    class Upstream(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            tag = body.get("input") or body["messages"][-1]["content"]
            calls.append({"path": self.path, "tag": tag,
                          "proof_forwarded": "X-Twork-Employee-Authorization" in self.headers})
            status = 503 if tag == "fail" else 200
            payload = {"error": {"message": "synthetic unavailable", "type": "server_error"}}
            if status == 200 and self.path.endswith("/responses"):
                payload = {"id": "resp_test", "status": "completed", "model": "employee-model",
                           "output": [{"type": "message", "role": "assistant", "content": [
                               {"type": "output_text", "text": "好"}]}],
                           "usage": {"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}}
            elif status == 200:
                payload = {"id": "chat_test", "object": "chat.completion", "model": "employee-model",
                           "choices": [{"index": 0, "message": {"role": "assistant", "content": "好"}, "finish_reason": "stop"}],
                           "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}}
            content = json.dumps(payload).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(content)))
            self.end_headers()
            self.wfile.write(content)

    upstream = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Upstream)
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    process = None
    with tempfile.TemporaryDirectory(prefix="employee-chat-bridge-") as temporary:
        root = Path(temporary)
        dbpath = root / "smoke.db"
        gateway_port = port()
        base = f"http://127.0.0.1:{gateway_port}"
        env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "PORT": str(gateway_port),
               "SQLITE_PATH": str(dbpath), "SQL_DSN": "", "LOG_SQL_DSN": "", "REDIS_CONN_STRING": "",
               "MEMORY_CACHE_ENABLED": "true", "SYNC_FREQUENCY": "1", "BATCH_UPDATE_ENABLED": "false",
               "GLOBAL_API_RATE_LIMIT_ENABLE": "false", "SESSION_SECRET": "employee-bridge-local-test",
               "GIN_MODE": "release", "TWORK_EMPLOYEE_CHAT_BRIDGE_KEY": key}
        with (args.output / "gateway.log").open("w") as log:
            def start():
                nonlocal process
                process = subprocess.Popen([str(args.binary.resolve())], cwd=root, env=env,
                                           stdout=log, stderr=subprocess.STDOUT)
                wait_until(lambda: request(base + "/api/status")[0] == 200, 45)

            def stop():
                nonlocal process
                if process is not None:
                    process.terminate()
                    process.wait(timeout=15)
                    process = None

            try:
                start()
                stop()
                with sqlite3.connect(dbpath) as db:
                    for uid, role in [(101, 100), (102, 1)]:
                        db.execute('INSERT INTO users (id,username,password,display_name,role,status,quota,"group",aff_code,setting) VALUES (?,?,?,?,?,?,?,?,?,?)',
                                   (uid, f"employee{uid}", "unused", f"employee{uid}", role, 1, 1000000, "default", f"employee{uid}", "{}"))
                    for tid, uid in [(201, 101), (202, 102)]:
                        db.execute('INSERT INTO tokens (id,user_id,key,status,name,expired_time,remain_quota,unlimited_quota,model_limits_enabled,model_limits,"group") VALUES (?,?,?,?,?,?,?,?,?,?,?)',
                                   (tid, uid, f"synthetictoken{tid}", 1, f"employee{tid}", -1, 1000000, 0, 1, "compile-only", "default"))
                    for cid, wire in [(301, "legacy"), (302, "responses"), (303, "chat_completions")]:
                        setting = {} if wire == "legacy" else {"twork_runtime": "pi", "twork_wire_api": wire}
                        db.execute('INSERT INTO channels (id,type,key,status,name,models,"group",base_url,priority,weight,auto_ban,ratio,setting,settings,channel_info) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)',
                                   (cid, 1, "syntheticupstream", 1, wire, "employee-model", "default",
                                    f"http://127.0.0.1:{upstream.server_port}/{cid}", 0, 30, 0, 1,
                                    json.dumps(setting), "{}", sqlite3.Binary(b"{}")))
                        db.execute('INSERT INTO abilities ("group",model,channel_id,enabled,priority,weight) VALUES (?,?,?,?,?,?)',
                                   ("default", "employee-model", cid, 1, 0, 30))
                    for name, value in {"ModelRatio": {"employee-model": 1}, "CompletionRatio": {"employee-model": 1}, "GroupRatio": {"default": 1}}.items():
                        db.execute('INSERT OR REPLACE INTO options (key,value) VALUES (?,?)', (name, json.dumps(value)))
                    db.execute('INSERT OR REPLACE INTO options (key,value) VALUES (?,?)', ("RetryTimes", "3"))
                start()

                def relay(cid, tag, token=201, sign=True, tamper=False):
                    path = "/v1/responses" if cid == 302 else "/v1/chat/completions"
                    body = {"model": "employee-model", "stream": False}
                    body.update({"input": tag} if cid == 302 else {"messages": [{"role": "user", "content": tag}]})
                    raw = json.dumps(body, ensure_ascii=False, separators=(",", ":"))
                    stamp = str(int(time.time()))
                    material = f"twork-employee-chat-v1\n{stamp}\nPOST\n{path}\n{token}\n{cid}\n{hashlib.sha256(raw.encode()).hexdigest()}"
                    digest = hmac.new(key.encode(), material.encode(), hashlib.sha256).hexdigest()
                    headers = {"Content-Type": "application/json", "Authorization": f"Bearer sk-synthetictoken{token}-{cid}"}
                    if sign:
                        headers["X-Twork-Employee-Authorization"] = stamp + "." + digest
                    return request(base + path, raw + (" " if tamper else ""), headers)

                for cid in [301, 302, 303]:
                    status, raw = relay(cid, f"success-{cid}")
                    assert status == 200, (cid, status, raw.decode())
                for kwargs in [{"sign": False}, {"tamper": True}, {"token": 202}]:
                    status, raw = relay(302, "denied", **kwargs)
                    assert status == 403, (kwargs, status, raw.decode())
                for cid in [301, 302]:
                    status, raw = relay(cid, "fail")
                    assert status == 503, (cid, status, raw.decode())
                assert len(calls) == 5, calls
                assert not any(call["proof_forwarded"] for call in calls), calls
                assert len([call for call in calls if call["tag"] == "fail"]) == 2

                def records():
                    with sqlite3.connect(dbpath) as db:
                        return db.execute('SELECT token_id,channel_id,quota FROM logs WHERE type=2 ORDER BY id').fetchall()

                wait_until(lambda: len(records()) == 3)
                consumed = records()
                assert [(r[0], r[1]) for r in consumed] == [(201, 301), (201, 302), (201, 303)], consumed
                assert all(r[2] > 0 for r in consumed), consumed
                with sqlite3.connect(dbpath) as db:
                    assert db.execute('SELECT COUNT(*) FROM token_model_channels').fetchone()[0] == 0
                    assert db.execute('SELECT model_limits FROM tokens WHERE id=201').fetchone()[0] == "compile-only"
                    assert db.execute('SELECT used_quota FROM tokens WHERE id=202').fetchone()[0] == 0
                result = {"result": "PASS", "synthetic_upstream_only": True,
                          "consume_logs": consumed, "upstream_calls": calls,
                          "personal_token_unchanged": True, "grants_unchanged": True,
                          "failure_attempts_per_channel": 1, "proof_forwarded": False}
                (args.output / "result.json").write_text(json.dumps(result, ensure_ascii=False, indent=2))
                print(json.dumps(result, ensure_ascii=False))
            finally:
                stop()
                upstream.shutdown()


if __name__ == "__main__":
    main()
