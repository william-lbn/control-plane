// SQL-free fixture for an explicitly owned Function; no package downloads.
export function createProbe(version, upgradeWebSocket) {
  if (!['A', 'B'].includes(version)) throw new Error('Fixed version required');
  const streams = new Map(), encoder = new TextEncoder();
  const json = (value, status = 200) => new Response(JSON.stringify({ version, ...value }), {
    status, headers: { 'content-type': 'application/json', 'cache-control': 'no-store' },
  });
  return async request => {
    const url = new URL(request.url), action = url.pathname.split('/').at(-1);
    if (action === 'http' && request.method === 'POST') {
      const body = await request.text();
      return Buffer.byteLength(body) <= 1024 ? json({ body }) : json({ error: 'body_limit' }, 413);
    }
    if (action === 'cpu' && request.method === 'GET') {
      let checksum = 0;
      for (let i = 0; i < 50000; i++) checksum = (Math.imul(checksum ^ i, 31) + 17) >>> 0;
      return json({ iterations: 50000, checksum });
    }
    if (action === 'sse' && request.method === 'GET') {
      const nonce = url.searchParams.get('nonce');
      if (!/^[a-f0-9]{32}$/.test(nonce ?? '')) return json({ error: 'nonce' }, 422);
      if (streams.has(nonce)) return json({ error: 'duplicate' }, 409);
      if (streams.size >= 4) return json({ error: 'stream_limit' }, 429);
      let cleanup;
      const body = new ReadableStream({
        start(controller) {
          let closed = false, timer;
          const finish = status => {
            if (closed) return;
            closed = true; clearTimeout(timer); streams.delete(nonce);
            request.signal.removeEventListener('abort', abort);
            try {
              if (status) controller.enqueue(encoder.encode('data: ' + JSON.stringify({ version, seq: 1, status }) + '\n\n'));
              controller.close();
            } catch { /* The original reader may already have cancelled. */ }
          };
          const abort = () => finish();
          cleanup = abort; streams.set(nonce, () => finish('released'));
          request.signal.addEventListener('abort', abort, { once: true });
          timer = setTimeout(() => finish('expired'), 10000);
          controller.enqueue(encoder.encode('data: ' + JSON.stringify({ version, seq: 0 }) + '\n\n'));
          if (request.signal.aborted) abort();
        },
        cancel() { cleanup?.(); },
      });
      return new Response(body, { headers: { 'content-type': 'text/event-stream', 'cache-control': 'no-store' } });
    }
    if (action === 'release' && request.method === 'POST') {
      const release = streams.get(url.searchParams.get('nonce'));
      if (!release) return json({ error: 'stream_missing' }, 404);
      release(); return json({ released: true });
    }
    if (action === 'ws' && request.method === 'GET') {
      const { socket, response } = upgradeWebSocket(request, { protocol: 'edge-probe.v1' });
      socket.onopen = () => socket.send(JSON.stringify({ version }));
      socket.onmessage = event => {
        if (typeof event.data === 'string') socket.send(version + ':' + event.data);
        else {
          const bytes = new Uint8Array(event.data), reply = new Uint8Array(bytes.length + 1);
          reply[0] = version.charCodeAt(0); reply.set(bytes, 1); socket.send(reply);
        }
      };
      return response;
    }
    return json({ error: 'probe_route' }, 404);
  };
}
