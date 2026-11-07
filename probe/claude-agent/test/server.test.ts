import assert from 'node:assert/strict';
import { mkdtempSync, rmSync } from 'node:fs';
import { createServer, IncomingMessage, RequestListener, ServerResponse } from 'node:http';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import { probeAnthropicMessages, runProbe } from '../src/server.js';

const ANTHROPIC_MESSAGE = {
  type: 'message',
  role: 'assistant',
  content: [{ type: 'text', text: 'OK' }],
  usage: { input_tokens: 5, output_tokens: 1 },
};

async function withUpstream(handler: RequestListener, run: (baseUrl: string) => Promise<void>) {
  const server = createServer(handler);
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const address = server.address();
  assert(address && typeof address === 'object');

  try {
    await run(`http://127.0.0.1:${address.port}`);
  } finally {
    server.closeAllConnections();
    await new Promise<void>((resolve) => server.close(() => resolve()));
  }
}

function answerWith(statusCode: number, contentType: string, body: string) {
  return (_req: IncomingMessage, res: ServerResponse) => {
    res.writeHead(statusCode, { 'content-type': contentType });
    res.end(body);
  };
}

function fallbackInput(baseUrl: string) {
  return { baseUrl, apiKey: 'test-key', model: 'gpt-5.5', timeoutMs: 1000 };
}

test('anthropic messages fallback succeeds on 2xx response', async () => {
  const server = createServer((req, res) => {
    assert.equal(req.method, 'POST');
    assert.equal(req.url, '/v1/messages');
    assert.equal(req.headers['x-api-key'], 'test-key');
    assert.equal(req.headers['anthropic-version'], '2023-06-01');

    res.writeHead(200, { 'content-type': 'application/json' });
    res.end(JSON.stringify(ANTHROPIC_MESSAGE));
  });

  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const address = server.address();
  assert.equal(typeof address, 'object');
  assert(address);

  try {
    const result = await probeAnthropicMessages({
      baseUrl: `http://127.0.0.1:${address.port}`,
      apiKey: 'test-key',
      authToken: '',
      model: 'kimi-k2.6',
      timeoutMs: 1000,
      customHeaders: undefined,
      prompt: 'Reply exactly OK.',
    });

    assert.equal(result.success, true);
    assert.equal(result.statusCode, 200);
  } finally {
    await new Promise<void>((resolve, reject) => {
      server.close((error) => {
        if (error) {
          reject(error);
          return;
        }
        resolve();
      });
    });
  }
});

test('anthropic messages fallback fails on non-2xx response', async () => {
  const server = createServer((_req, res) => {
    res.writeHead(401, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ error: { message: 'invalid api key' } }));
  });

  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const address = server.address();
  assert.equal(typeof address, 'object');
  assert(address);

  try {
    const result = await probeAnthropicMessages({
      baseUrl: `http://127.0.0.1:${address.port}`,
      apiKey: 'bad-key',
      authToken: '',
      model: 'kimi-k2.6',
      timeoutMs: 1000,
      customHeaders: undefined,
      prompt: 'Reply exactly OK.',
    });

    assert.equal(result.success, false);
    assert.equal(result.statusCode, 401);
    assert.match(result.error || '', /status 401/);
  } finally {
    await new Promise<void>((resolve, reject) => {
      server.close((error) => {
        if (error) {
          reject(error);
          return;
        }
        resolve();
      });
    });
  }
});

test('should request /v1/messages when channel base url has no /v1 suffix', async () => {
  const paths: string[] = [];
  const handler: RequestListener = (req, res) => {
    paths.push(req.url || '');
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end(JSON.stringify(ANTHROPIC_MESSAGE));
  };

  await withUpstream(handler, async (baseUrl) => {
    const result = await probeAnthropicMessages(fallbackInput(`${baseUrl}/proxy/`));

    assert.equal(result.success, true);
    assert.deepEqual(paths, ['/proxy/v1/messages']);
  });
});

test('should fail fallback when upstream returns 200 html page', async () => {
  const handler = answerWith(200, 'text/html; charset=utf-8', '<!doctype html><html></html>');

  await withUpstream(handler, async (baseUrl) => {
    const result = await probeAnthropicMessages(fallbackInput(baseUrl));

    assert.equal(result.success, false);
  });
});

test('should fail fallback when upstream returns 200 json without a message', async () => {
  const handler = answerWith(200, 'application/json', JSON.stringify({ type: 'error', error: {} }));

  await withUpstream(handler, async (baseUrl) => {
    const result = await probeAnthropicMessages(fallbackInput(baseUrl));

    assert.equal(result.success, false);
    assert.equal(result.statusCode, 200);
  });
});

test('should fail fallback when upstream returns an empty message', async () => {
  const body = JSON.stringify({ type: 'message', content: [], usage: { input_tokens: 5, output_tokens: 0 } });

  await withUpstream(answerWith(200, 'application/json', body), async (baseUrl) => {
    const result = await probeAnthropicMessages(fallbackInput(baseUrl));

    assert.equal(result.success, false);
  });
});

test('should pass fallback when message has output tokens but no content blocks', async () => {
  const body = JSON.stringify({ type: 'message', content: [], usage: { input_tokens: 5, output_tokens: 1 } });

  await withUpstream(answerWith(200, 'application/json', body), async (baseUrl) => {
    const result = await probeAnthropicMessages(fallbackInput(baseUrl));

    assert.equal(result.success, true);
  });
});

test('should report upstream 503 quickly without retrying when /messages returns 200 html', async () => {
  const home = mkdtempSync(join(tmpdir(), 'claude-agent-probe-test-'));
  const previousHome = process.env.HOME;
  process.env.HOME = home;
  process.env.NO_PROXY = '127.0.0.1';
  let messageRequests = 0;
  const handler: RequestListener = (req, res) => {
    if (req.url?.startsWith('/v1/messages')) {
      messageRequests += 1;
      res.writeHead(503, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ type: 'error', error: { type: 'api_error', message: 'no channel' } }));
      return;
    }
    res.writeHead(200, { 'content-type': 'text/html' });
    res.end('<!doctype html><html></html>');
  };

  try {
    await withUpstream(handler, async (baseUrl) => {
      const result = await runProbe({ base_url: baseUrl, api_key: 'test-key', model: 'gpt-5.5', timeout_ms: 30000 });

      assert.equal(result.success, false);
      assert.match(result.error || '', /503/);
      assert(result.latency_ms < 25000, `probe took ${result.latency_ms}ms`);
      // Claude Code 一次，回退检查一次，多出来的就是重试。
      assert.equal(messageRequests, 2);
    });
  } finally {
    process.env.HOME = previousHome;
    rmSync(home, { recursive: true, force: true });
  }
});

test('should fail probe when claude code gets an empty reply and fallback is empty too', async () => {
  const home = mkdtempSync(join(tmpdir(), 'claude-agent-probe-test-'));
  const previousHome = process.env.HOME;
  process.env.HOME = home;
  process.env.NO_PROXY = '127.0.0.1';
  const emptyMessage = { id: 'msg_1', type: 'message', role: 'assistant', model: 'gpt-5.5', content: [], stop_reason: 'end_turn', stop_sequence: null };
  const handler: RequestListener = (req, res) => {
    if (!req.url?.startsWith('/v1/messages')) {
      res.writeHead(404);
      res.end();
      return;
    }
    let raw = '';
    req.on('data', (chunk) => {
      raw += chunk;
    });
    req.on('end', () => {
      if (!JSON.parse(raw).stream) {
        res.writeHead(200, { 'content-type': 'application/json' });
        res.end(JSON.stringify({ ...emptyMessage, usage: { input_tokens: 5, output_tokens: 0 } }));
        return;
      }
      res.writeHead(200, { 'content-type': 'text/event-stream' });
      const events: [string, object][] = [
        ['message_start', { type: 'message_start', message: { ...emptyMessage, stop_reason: null, usage: { input_tokens: 5, output_tokens: 0 } } }],
        ['message_delta', { type: 'message_delta', delta: { stop_reason: 'end_turn', stop_sequence: null }, usage: { output_tokens: 0 } }],
        ['message_stop', { type: 'message_stop' }],
      ];
      for (const [name, data] of events) {
        res.write(`event: ${name}\ndata: ${JSON.stringify(data)}\n\n`);
      }
      res.end();
    });
  };

  try {
    await withUpstream(handler, async (baseUrl) => {
      const result = await runProbe({ base_url: baseUrl, api_key: 'test-key', model: 'gpt-5.5', timeout_ms: 30000 });

      assert.equal(result.success, false);
    });
  } finally {
    process.env.HOME = previousHome;
    rmSync(home, { recursive: true, force: true });
  }
});
