import http from 'node:http';
import { once } from 'node:events';
import { pathToFileURL } from 'node:url';
import { invocationContext } from './context.mjs';

const maxRequestBytes = 1024 * 1024;
const maxConcurrency = 16;
const durationMs = 15 * 60 * 1000;
const excludedHeaders = new Set(['connection', 'keep-alive', 'transfer-encoding', 'upgrade', 'proxy-authenticate', 'proxy-authorization', 'te', 'trailer', 'x-neon-request-url']);

// Only the root supervisor reaches this loopback server. Guest Node code has
// no manager key, platform Secret mount or Kubernetes ServiceAccount token.
export async function startRuntime({ entry, port = 8081, deadlineMs = durationMs, clock = () => Date.now() }) {
  if (!Number.isInteger(deadlineMs) || deadlineMs < 10 || deadlineMs > durationMs) throw new Error('Invalid invocation deadline');
  const module = await import(pathToFileURL(entry).href);
  const handler = typeof module.default === 'function' ? module.default : module.default?.fetch?.bind(module.default);
  if (typeof handler !== 'function') throw new Error('Function requires a default fetch handler');
  let active = 0, pending = 0, failures = 0, draining = false, lastActivity = clock();
  const requests = new Set();

  const server = http.createServer(async (incoming, outgoing) => {
    if (incoming.url === '/_neon/status' && incoming.method === 'GET') {
      outgoing.writeHead(200, { 'content-type': 'application/json', 'cache-control': 'no-store' });
      outgoing.end(JSON.stringify({ ready: !draining, active, pending, failures, last_activity_ms: lastActivity }));
      return;
    }
    if (draining || active + pending >= maxConcurrency) {
      outgoing.writeHead(429, { 'retry-after': '1' }); outgoing.end('Function capacity unavailable'); return;
    }
    if (incoming.headers.upgrade) { outgoing.writeHead(426); outgoing.end('WebSocket is not enabled in this runtime slice'); return; }
    const controller = new AbortController();
    requests.add(controller); active++; lastActivity = clock();
    let finished = false;
    const state = { pending: [], closed: false };
    const timer = setTimeout(() => controller.abort(new Error('Invocation deadline reached')), deadlineMs);
    const abortResponse = () => { if (!outgoing.writableFinished) outgoing.destroy(); };
    controller.signal.addEventListener('abort', abortResponse, { once: true });
    const clientClosed = () => { if (!outgoing.writableFinished) controller.abort(new Error('Caller disconnected')); };
    outgoing.on('close', clientClosed);
    try {
      const target = incoming.headers['x-neon-request-url'];
      let url;
      try { url = new URL(target); } catch { throw new Error('Supervisor did not provide an invocation URL'); }
      if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password) throw new Error('Invalid invocation URL');
      const headers = new Headers();
      for (const [key, value] of Object.entries(incoming.headers)) {
        if (excludedHeaders.has(key) || key === 'host' || value === undefined) continue;
        if (Array.isArray(value)) { for (const part of value) headers.append(key, part); }
        else headers.set(key, value);
      }
      const chunks = []; let bytes = 0;
      for await (const chunk of incoming) {
        bytes += chunk.length;
        if (bytes > maxRequestBytes) {
          outgoing.writeHead(413); outgoing.end('Request body limit exceeded'); finished = true; return;
        }
        chunks.push(chunk);
      }
      const request = new Request(url, { method: incoming.method, headers, signal: controller.signal,
        ...(!['GET', 'HEAD'].includes(incoming.method) ? { body: Buffer.concat(chunks) } : {}) });
      let aborted;
      const abortPromise = new Promise((_, reject) => {
        aborted = () => reject(new Error('Invocation cancelled'));
        controller.signal.addEventListener('abort', aborted, { once: true });
        if (controller.signal.aborted) aborted();
      });
      try {
        await invocationContext.run(state, async () => {
          const response = await Promise.race([handler(request), abortPromise]);
          if (!(response instanceof Response) || response.status < 200 || response.status > 599) throw new Error('Handler must return a web Response');
          const responseHeaders = {};
          for (const [key, value] of response.headers) if (!excludedHeaders.has(key) && key !== 'set-cookie') responseHeaders[key] = value;
          const cookies = response.headers.getSetCookie();
          if (cookies.length) responseHeaders['set-cookie'] = cookies;
          outgoing.writeHead(response.status, responseHeaders);
          if (response.body && incoming.method !== 'HEAD') {
            const reader = response.body.getReader();
            try {
              while (true) {
                const part = await Promise.race([reader.read(), abortPromise]);
                if (part.done) break;
                if (!outgoing.write(part.value)) await Promise.race([once(outgoing, 'drain'), abortPromise]);
              }
            } finally { await reader.cancel().catch(() => {}); }
          } else if (response.body) { await response.body.cancel(); }
          outgoing.end(); finished = true;
        });
        // A completed response does not let a VM disappear while registered
        // background work is pending. The root supervisor enforces its own
        // deadline and process/cgroup boundary independently of Node counters.
        if (state.pending.length) {
          active--; pending++;
          try {
            const results = await Promise.race([Promise.all(state.pending), abortPromise]);
            failures += results.filter(success => !success).length;
          } finally { pending--; active++; }
        }
      } finally { controller.signal.removeEventListener('abort', aborted); }
    } catch {
      failures++;
      if (!outgoing.headersSent && !outgoing.destroyed) { outgoing.writeHead(502); outgoing.end('Function execution failed'); }
      else if (!finished) outgoing.destroy();
    } finally {
      state.closed = true; active--; lastActivity = clock(); requests.delete(controller); clearTimeout(timer);
      controller.signal.removeEventListener('abort', abortResponse); outgoing.off('close', clientClosed);
    }
  });
  server.requestTimeout = durationMs;
  server.headersTimeout = 10_000;
  server.keepAliveTimeout = 1000;
  server.maxHeadersCount = 64;
  await new Promise((resolve, reject) => { server.once('error', reject); server.listen(port, '127.0.0.1', resolve); });
  return { server, status: () => ({ active, pending, failures, lastActivity }),
    async close() {
      draining = true;
      for (const request of requests) request.abort(new Error('Runtime draining'));
      server.closeIdleConnections();
      await new Promise(resolve => server.close(resolve));
    } };
}
