#!/usr/bin/env python3
"""用发布二进制、临时 SQLite 和合成上游验证 Responses 切换，完全隔离生产数据。"""
import argparse
import hashlib
import http.server
import http.client
import json
import os
from pathlib import Path
import shutil
import sqlite3
import subprocess
import tempfile
import threading
import time
from verify_twork_model_routes import port, request, wait_until

MODEL = 'gpt-synthetic-failover'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    binary = args.binary.resolve()
    args.output.mkdir(parents=True, exist_ok=True)
    calls, failed = [], set()

    class Upstream(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            if self.headers.get('Transfer-Encoding', '').lower() == 'chunked':
                chunks = []
                while True:
                    size = int(self.rfile.readline().split(b';', 1)[0], 16)
                    if size == 0:
                        while self.rfile.readline().strip():
                            pass
                        break
                    chunks.append(self.rfile.read(size))
                    assert self.rfile.read(2) == b'\r\n'
                raw = b''.join(chunks)
            else:
                raw = self.rfile.read(int(self.headers['Content-Length']))
            body = json.loads(raw)
            channel = int(self.path.split('/')[1])
            calls.append(channel)
            tag = body.get('input')
            partial = tag == 'partial' and channel == 301
            status = 503 if channel in failed and not partial else 200
            usage = {'input_tokens': 10, 'output_tokens': 5, 'total_tokens': 15}
            result = {'id': 'synthetic', 'object': 'response', 'status': 'completed', 'model': MODEL,
                      'output': [{'type': 'message', 'role': 'assistant', 'status': 'completed',
                                  'content': [{'type': 'output_text', 'text': 'OK', 'annotations': []}]}],
                      'usage': usage}
            content_type = 'application/json'
            if status == 503:
                result = {'error': {'type': 'server_error', 'message': 'synthetic unavailable'}}
            if status == 200 and body.get('stream'):
                content_type = 'text/event-stream'
                terminal = 'response.completed'
                if partial:
                    result.update(status='failed', error={'code': 'server_error', 'message': 'synthetic failure'})
                    terminal = 'response.failed'
                events = [{'type': 'response.output_text.delta', 'delta': 'partial' if partial else 'OK'},
                          {'type': terminal, 'response': result}]
                payload = ''.join('data: ' + json.dumps(event) + '\n\n' for event in events).encode()
            else:
                payload = json.dumps(result).encode()
            self.send_response(status)
            self.send_header('Content-Type', content_type)
            self.send_header('Content-Length', str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

    server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Upstream)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    root = Path(tempfile.mkdtemp(prefix='responses-acceptance-'))
    dbpath = root / 'test.db'
    base = f'http://127.0.0.1:{port()}'
    env = {'PATH': os.environ.get('PATH', '/usr/bin:/bin'), 'HOME': str(root),
           'PORT': base.rsplit(':', 1)[1], 'SQLITE_PATH': str(dbpath), 'SQL_DSN': '', 'LOG_SQL_DSN': '',
           'REDIS_CONN_STRING': '', 'MEMORY_CACHE_ENABLED': 'true', 'SYNC_FREQUENCY': '1',
           'BATCH_UPDATE_ENABLED': 'false', 'GLOBAL_API_RATE_LIMIT_ENABLE': 'false',
           'SESSION_SECRET': 'isolated-responses-acceptance', 'GIN_MODE': 'release'}
    process = None
    connection = http.client.HTTPConnection("127.0.0.1", int(base.rsplit(":",1)[1]), timeout=20)
    logfile = (args.output / 'gateway.log').open('w')

    def start():
        nonlocal process
        process = subprocess.Popen([str(binary)], cwd=root, env=env, stdout=logfile, stderr=subprocess.STDOUT)
        wait_until(lambda: request(base + '/api/status')[0] == 200, 45)

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

    def relay(session, tag='normal', stream=False, fixed=False):
        calls.clear()
        headers = {'Authorization': 'Bearer sk-syntheticresponses', 'Content-Type': 'application/json',
                   'X-Twork-Client-Version': '4.0.0', 'X-Twork-Client-Capabilities': 'model-routes-v3',
                   'X-Twork-Route-Mode': 'model', 'X-Twork-Agent-Runtime': 'pi', 'X-Twork-Wire-Api': 'responses'}
        if fixed:
            headers.pop('X-Twork-Route-Mode')
            headers['X-Twork-Channel-Id'] = '301'
            headers['X-Twork-Client-Capabilities'] = 'model-routes-v2'
        # 与桌面 SDK 一样复用连接，避免 urllib 的 Connection: close 在结算前取消服务端上下文。
        connection.request('POST', '/v1/responses', json.dumps({'model': MODEL, 'input': tag, 'store': False,
                                                               'prompt_cache_key': session, 'stream': stream}), headers)
        response = connection.getresponse()
        return response.status, response.read()

    try:
        start()
        stop()
        with sqlite3.connect(dbpath) as db:
            db.execute('INSERT INTO users (id,username,password,display_name,role,status,quota,"group",aff_code,setting) VALUES (101,?,?,?,?,?,?,?,?,?)',
                       ('syntheticresponses', 'unused', 'synthetic', 1, 1, 1000000, 'default', 'synthetic', '{}'))
            db.execute('INSERT INTO tokens (id,user_id,key,status,name,expired_time,remain_quota,unlimited_quota,model_limits_enabled,model_limits,"group") VALUES (201,101,?,1,?,-1,1000000,0,0,?,?)',
                       ('syntheticresponses', 'synthetic', '', 'default'))
            for channel in (301, 302):
                settings = json.dumps({'twork_runtime': 'pi', 'twork_wire_api': 'responses', 'pass_through_body_enabled': True})
                db.execute('INSERT INTO channels (id,type,key,status,name,models,"group",base_url,priority,weight,auto_ban,ratio,setting,settings,channel_info) VALUES (?,1,?,1,?,?,?,?,?,1,1,1,?,?,?)',
                           (channel, 'synthetic', str(channel), MODEL, 'default', f'http://127.0.0.1:{server.server_port}/{channel}', 603-channel, settings, '{}', sqlite3.Binary(b'{}')))
                db.execute('INSERT INTO abilities ("group",model,channel_id,enabled,priority,weight) VALUES (?,?,?,1,?,1)', ('default', MODEL, channel, 603-channel))
                db.execute('INSERT INTO token_model_channels (token_id,model_id,channel_id) VALUES (201,?,?)', (MODEL, channel))
            affinity = {'enabled': True, 'switch_on_success': True, 'rules': [{'name': 'synthetic responses', 'model_regex': ['^gpt-.*$'],
                        'path_regex': ['/v1/responses'], 'key_sources': [{'type': 'gjson', 'path': 'prompt_cache_key'}],
                        'include_model_name': True, 'ttl_seconds': 3600, 'skip_retry_on_failure': True}]}
            options = {'RetryTimes': '2', 'channel_health_setting.soft_failure_cooldown_enabled': 'true',
                       'channel_health_setting.soft_failure_cooldown_seconds': '1', 'channel_health_setting.soft_failure_max_attempts': '3',
                       'channel_affinity_setting': json.dumps(affinity)}
            for key, value in affinity.items():
                options['channel_affinity_setting.'+key] = json.dumps(value)
            for key in ('ModelRatio', 'CompletionRatio'):
                options[key] = json.dumps({MODEL: 1})
            options['GroupRatio'] = json.dumps({'default': 1})
            for key, value in options.items():
                db.execute('INSERT OR REPLACE INTO options (key,value) VALUES (?,?)', (key, value))
        start()
        version = json.loads(request(base + '/api/status')[1])['data']['version']
        assert relay('session-a')[0] == 200 and calls == [301], calls
        failed.add(301)
        status, body = relay('session-a')
        assert status == 200 and calls == [301, 302], (status, calls, body)
        assert relay('session-b')[0] == 200 and calls == [302], calls
        failed.clear()
        # 真正等待配置的冷却窗口，用发布二进制验证时间语义。
        time.sleep(1.2)
        assert relay('session-a')[0] == 200 and calls == [302], calls
        assert relay('session-c')[0] == 200 and calls == [301], calls
        status, body = relay('session-c', 'partial', stream=True)
        assert status == 200 and calls == [301] and b'partial' in body and b'"type":"error"' in body, (status, calls, body)
        assert relay('session-c')[0] == 200 and calls == [302], calls
        failed.add(301)
        status, body = relay('fixed', fixed=True)
        assert status == 503 and calls == [301], (status, calls, body)
        failed.add(302)
        status, body = relay('all-failed')
        assert status == 503, (status, body)
        status, body = relay('all-cooling')
        assert status == 503 and calls == [], (status, calls, body)

        def logs():
            with sqlite3.connect(dbpath) as db:
                return db.execute('SELECT channel_id,quota,prompt_tokens,completion_tokens FROM logs WHERE token_id=201 AND type=2 ORDER BY id').fetchall()
        wait_until(lambda: len(logs()) == 7)
        consumed = logs()
        assert [row[0] for row in consumed] == [301,302,302,302,301,301,302], consumed
        assert all(row[1] > 0 and row[2:] == (10,5) for row in consumed), consumed
        with sqlite3.connect(dbpath) as db:
            assert db.execute('SELECT COUNT(*) FROM channel_model_disabled').fetchone()[0] == 0
        result = {'result':'PASS','version':version,'binary_sha256':hashlib.sha256(binary.read_bytes()).hexdigest(),
                  'synthetic_only':True,'priority_failover':True,'sticky_after_recovery':True,'cooldown':True,
                  'partial_stream_no_retry':True,'fixed_channel_no_retry':True,'no_permanent_disable':True,
                  'consume_logs':consumed,'no_duplicate_billing':True}
        (args.output/'result.json').write_text(json.dumps(result,indent=2))
        print(json.dumps(result))
    finally:
        connection.close()
        stop()
        logfile.close()
        server.shutdown()
        server.server_close()
        shutil.rmtree(root)


if __name__ == '__main__':
    main()
