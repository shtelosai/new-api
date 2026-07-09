import { createServer, IncomingMessage, ServerResponse } from 'node:http';
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';
import { query } from '@anthropic-ai/claude-agent-sdk';

type ProbeRequest = {
  base_url?: string;
  api_key?: string;
  auth_token?: string;
  model?: string;
  timeout_ms?: number;
  custom_headers?: Record<string, string>;
  prompt?: string;
};

type ProbeResponse = {
  success: boolean;
  latency_ms: number;
  error?: string;
  local_error?: boolean;
  raw_error_type?: string;
  sdk_session_id?: string;
  output?: string;
};

export type AnthropicMessagesProbeInput = {
  baseUrl: string;
  apiKey?: string;
  authToken?: string;
  model: string;
  timeoutMs: number;
  customHeaders?: Record<string, string>;
  prompt?: string;
};

export type AnthropicMessagesProbeResult = {
  success: boolean;
  statusCode?: number;
  error?: string;
};

const port = Number(process.env.PORT || '3021');
const probeToken = process.env.PROBE_TOKEN || '';
const defaultTimeoutMs = Number(process.env.DEFAULT_PROBE_TIMEOUT_MS || '20000');
const require = createRequire(import.meta.url);

function writeJson(res: ServerResponse, statusCode: number, payload: unknown) {
  const body = JSON.stringify(payload);
  res.writeHead(statusCode, {
    'Content-Type': 'application/json; charset=utf-8',
    'Content-Length': Buffer.byteLength(body),
  });
  res.end(body);
}

function readBody(req: IncomingMessage): Promise<string> {
  return new Promise((resolve, reject) => {
    let body = '';
    req.setEncoding('utf8');
    req.on('data', (chunk) => {
      body += chunk;
      if (body.length > 1024 * 1024) {
        reject(new Error('request body too large'));
        req.destroy();
      }
    });
    req.on('end', () => resolve(body));
    req.on('error', reject);
  });
}

function isAuthorized(req: IncomingMessage): boolean {
  if (!probeToken) {
    return true;
  }
  const auth = req.headers.authorization || '';
  const token = Array.isArray(auth) ? auth[0] : auth;
  if (token === `Bearer ${probeToken}`) {
    return true;
  }
  const headerToken = req.headers['x-probe-token'];
  return headerToken === probeToken;
}

function normalizeBaseUrl(value: string): string {
  return value.trim().replace(/\/+$/, '');
}

function headersToEnv(headers?: Record<string, string>): string | undefined {
  if (!headers) {
    return undefined;
  }
  const lines = Object.entries(headers)
    .map(([key, value]) => [key.trim(), String(value).trim()] as const)
    .filter(([key, value]) => key && value)
    .map(([key, value]) => `${key}: ${value}`);
  return lines.length > 0 ? lines.join('\n') : undefined;
}

function validateRequest(payload: ProbeRequest): string | undefined {
  if (!payload.base_url || !payload.base_url.trim()) {
    return 'base_url is required';
  }
  if (!payload.model || !payload.model.trim()) {
    return 'model is required';
  }
  if (!payload.api_key?.trim() && !payload.auth_token?.trim()) {
    return 'api_key or auth_token is required';
  }
  return undefined;
}

function sdkErrorMessage(error: unknown): string {
  if (error instanceof Error) {
    return error.message;
  }
  if (typeof error === 'string') {
    return error;
  }
  try {
    return JSON.stringify(error);
  } catch {
    return 'unknown sdk error';
  }
}

function resolveClaudeCodeExecutable(): string | undefined {
  const extension = process.platform === 'win32' ? '.exe' : '';
  const candidates: string[] = [];

  if (process.platform === 'linux') {
    const baseName = `@anthropic-ai/claude-agent-sdk-linux-${process.arch}`;
    const report = process.report?.getReport?.() as { header?: { glibcVersionRuntime?: string } } | undefined;
    const hasGlibc = Boolean(report?.header?.glibcVersionRuntime);
    if (hasGlibc) {
      candidates.push(baseName);
    }
    candidates.push(`${baseName}-musl`);
    if (!hasGlibc) {
      candidates.push(baseName);
    }
  } else {
    candidates.push(`@anthropic-ai/claude-agent-sdk-${process.platform}-${process.arch}`);
  }

  for (const packageName of candidates) {
    try {
      return require.resolve(`${packageName}/claude${extension}`);
    } catch {
      // 继续尝试下一个平台包，最终交给 SDK 自身 fallback。
    }
  }
  return undefined;
}

function isLocalSdkError(errorMessage: string): boolean {
  const normalized = errorMessage.toLowerCase();
  return [
    'native cli binary',
    'native binary not found',
    'claude code executable not found',
    'failed to spawn claude code process',
    'cannot find module',
  ].some((needle) => normalized.includes(needle));
}

function buildAnthropicMessagesHeaders(input: AnthropicMessagesProbeInput): Record<string, string> {
  const headers: Record<string, string> = {
    'content-type': 'application/json',
    'anthropic-version': '2023-06-01',
    ...(input.customHeaders || {}),
  };

  if (input.apiKey?.trim()) {
    headers['x-api-key'] = input.apiKey.trim();
  }
  if (input.authToken?.trim()) {
    headers.authorization = `Bearer ${input.authToken.trim()}`;
  }

  return headers;
}

export async function probeAnthropicMessages(
  input: AnthropicMessagesProbeInput,
): Promise<AnthropicMessagesProbeResult> {
  const url = `${normalizeBaseUrl(input.baseUrl)}/messages`;
  const body = JSON.stringify({
    model: input.model,
    max_tokens: 1,
    messages: [{ role: 'user', content: input.prompt || 'Reply with exactly OK.' }],
  });

  try {
    const resp = await fetch(url, {
      method: 'POST',
      headers: buildAnthropicMessagesHeaders(input),
      body,
      signal: AbortSignal.timeout(Math.max(1000, input.timeoutMs)),
    });

    await resp.text();
    if (resp.status >= 200 && resp.status < 300) {
      return { success: true, statusCode: resp.status };
    }
    return {
      success: false,
      statusCode: resp.status,
      error: `anthropic messages probe returned status ${resp.status}`,
    };
  } catch (error) {
    return {
      success: false,
      error: `anthropic messages probe failed: ${sdkErrorMessage(error)}`,
    };
  }
}

async function runProbe(payload: ProbeRequest): Promise<ProbeResponse> {
  const startedAt = Date.now();
  const timeoutMs = Math.max(1000, payload.timeout_ms || defaultTimeoutMs);
  const abortController = new AbortController();
  const timer = setTimeout(() => abortController.abort(), timeoutMs);
  let sdkSessionId = '';
  let output = '';

  try {
    const baseUrl = normalizeBaseUrl(payload.base_url || '');
    const model = (payload.model || '').trim();
    const customHeaders = headersToEnv(payload.custom_headers);
    const env: Record<string, string | undefined> = {
      ...process.env,
      ANTHROPIC_BASE_URL: baseUrl,
      ANTHROPIC_MODEL: model,
      ANTHROPIC_SMALL_FAST_MODEL: model,
      ANTHROPIC_CUSTOM_MODEL_OPTION: model,
      ANTHROPIC_CUSTOM_MODEL_OPTION_NAME: model,
    };

    if (payload.api_key?.trim()) {
      env.ANTHROPIC_API_KEY = payload.api_key.trim();
    }
    if (payload.auth_token?.trim()) {
      env.ANTHROPIC_AUTH_TOKEN = payload.auth_token.trim();
    }
    if (customHeaders) {
      env.ANTHROPIC_CUSTOM_HEADERS = customHeaders;
    }

    const stream = query({
      prompt: payload.prompt || 'Reply with exactly OK.',
      options: {
        abortController,
        cwd: process.cwd(),
        disallowedTools: [
          'Agent',
          'Task',
          'Bash',
          'Read',
          'Edit',
          'Write',
          'Glob',
          'Grep',
          'WebFetch',
          'WebSearch',
          'TodoWrite',
        ],
        env,
        maxTurns: 1,
        model,
        pathToClaudeCodeExecutable: resolveClaudeCodeExecutable(),
        permissionMode: 'dontAsk',
        persistSession: false,
        promptSuggestions: false,
        settings: {
          autoCompactEnabled: false,
          alwaysThinkingEnabled: false,
          promptSuggestionEnabled: false,
        },
        settingSources: [],
        thinking: { type: 'disabled' },
        title: 'new-api-health-probe',
        tools: [],
      },
    });

    for await (const message of stream) {
      const msg = message as any;
      if (msg.session_id && !sdkSessionId) {
        sdkSessionId = String(msg.session_id);
      }
      if (msg.type === 'assistant' && Array.isArray(msg.message?.content)) {
        for (const part of msg.message.content) {
          if (part?.type === 'text' && part.text) {
            output += part.text;
          }
        }
      }
      if (msg.type === 'result' && msg.subtype === 'error_max_turns') {
        break;
      }
    }

    return {
      success: true,
      latency_ms: Date.now() - startedAt,
      sdk_session_id: sdkSessionId || undefined,
      output: output.trim() || undefined,
    };
  } catch (error) {
    const message = sdkErrorMessage(error);
    const localError = isLocalSdkError(message);
    if (!localError) {
      // SDK 可能因返回体不符合 Claude Code 预期而失败；监控只要求 Anthropic 协议连通。
      const fallback = await probeAnthropicMessages({
        baseUrl: payload.base_url || '',
        apiKey: payload.api_key,
        authToken: payload.auth_token,
        model: payload.model || '',
        timeoutMs: Math.max(1000, timeoutMs - (Date.now() - startedAt)),
        customHeaders: payload.custom_headers,
        prompt: payload.prompt,
      });
      if (fallback.success) {
        return {
          success: true,
          latency_ms: Date.now() - startedAt,
          sdk_session_id: sdkSessionId || undefined,
          output: `anthropic messages probe returned ${fallback.statusCode}`,
        };
      }
    }

    return {
      success: false,
      latency_ms: Date.now() - startedAt,
      error: message,
      local_error: localError,
      raw_error_type: error instanceof Error ? error.name : typeof error,
      sdk_session_id: sdkSessionId || undefined,
    };
  } finally {
    clearTimeout(timer);
  }
}

async function handleProbe(req: IncomingMessage, res: ServerResponse) {
  if (!isAuthorized(req)) {
    writeJson(res, 401, { success: false, latency_ms: 0, error: 'unauthorized' });
    return;
  }

  let payload: ProbeRequest;
  try {
    payload = JSON.parse(await readBody(req)) as ProbeRequest;
  } catch (error) {
    writeJson(res, 400, {
      success: false,
      latency_ms: 0,
      error: `invalid json: ${sdkErrorMessage(error)}`,
    });
    return;
  }

  const validationError = validateRequest(payload);
  if (validationError) {
    writeJson(res, 400, { success: false, latency_ms: 0, error: validationError });
    return;
  }

  writeJson(res, 200, await runProbe(payload));
}

const server = createServer(async (req, res) => {
  if (req.method === 'GET' && req.url === '/health') {
    writeJson(res, 200, { success: true });
    return;
  }
  if (req.method === 'POST' && req.url === '/probe') {
    await handleProbe(req, res);
    return;
  }
  writeJson(res, 404, { success: false, error: 'not found' });
});

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  server.listen(port, '0.0.0.0', () => {
    console.log(`claude-agent-probe listening on ${port}`);
  });
}
