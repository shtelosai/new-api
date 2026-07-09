import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import test from 'node:test';

import { probeAnthropicMessages } from '../src/server.js';

test('anthropic messages fallback succeeds on 2xx response', async () => {
  const server = createServer((req, res) => {
    assert.equal(req.method, 'POST');
    assert.equal(req.url, '/v1/messages');
    assert.equal(req.headers['x-api-key'], 'test-key');
    assert.equal(req.headers['anthropic-version'], '2023-06-01');

    res.writeHead(200, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ type: 'message', content: [] }));
  });

  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const address = server.address();
  assert.equal(typeof address, 'object');
  assert(address);

  try {
    const result = await probeAnthropicMessages({
      baseUrl: `http://127.0.0.1:${address.port}/v1`,
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
      baseUrl: `http://127.0.0.1:${address.port}/v1`,
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
