// Synthetic protocol fixture. State lives on its own disposable volume. Never
// connect this service to a commercial supplier or use customer prompts.
import http from 'node:http';
import { readFileSync, writeFileSync, renameSync, mkdirSync } from 'node:fs';
if (process.platform !== 'linux') throw Error('Linux provider-state fixture required');
const dir = process.env.NEON_PROVIDER_FIXTURE_DIR || '/run/state';
mkdirSync(dir, { recursive: true, mode: 0o700 });
const file = dir + '/state.private.json';
let state;
try {
  state = JSON.parse(readFileSync(file, 'utf8'));
} catch (e) {
  if (e.code !== 'ENOENT') throw e;
  state = {
    serial: 0,
    objects: {},
    items: {},
    calls: { responses: 0, conversations: 0, items: 0, reads: 0, deletes: 0 },
  };
}
const save = () => {
  writeFileSync(file + '.tmp', JSON.stringify(state), { mode: 0o600 });
  renameSync(file + '.tmp', file);
};
const reply = (res, status, value) => {
  res.writeHead(status, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify(value));
};
const list = (data) => ({
  object: 'list',
  data,
  first_id: data[0]?.id || null,
  last_id: data.at(-1)?.id || null,
  has_more: false,
});
const items = (input) => input.map((value) => ({ ...value, id: 'msg_fixture' + ++state.serial }));
const proof = (value) => /^[a-f0-9]{64}$/.test(value?.metadata?._neon_binding || '');
http
  .createServer(async (req, res) => {
    const url = new URL(req.url, 'http://fixture');
    if (req.method === 'GET' && url.pathname === '/healthz')
      return reply(res, 200, { synthetic: true, ok: true });
    if (req.method === 'GET' && url.pathname === '/stats')
      return reply(res, 200, {
        synthetic: true,
        calls: state.calls,
        resources: Object.keys(state.objects).length,
      });
    if (req.headers.authorization || req.headers.cookie)
      return reply(res, 400, { error: 'incoming credential escaped' });
    let body = {};
    try {
      if (req.method === 'POST') {
        const chunks = [];
        let size = 0;
        for await (const chunk of req) {
          size += chunk.length;
          if (size > 2 * 1024 * 1024) throw Error('too large');
          chunks.push(chunk);
        }
        body = JSON.parse(Buffer.concat(chunks).toString());
      }
      const path = url.pathname.replace(/^\/v1\//, ''),
        parts = path.split('/');
      if (req.method === 'POST' && path === 'conversations') {
        if (!proof(body) || Object.keys(state.objects).length > 64) throw Error('unbound creation');
        const id = 'conv_fixture' + ++state.serial,
          value = {
            id,
            object: 'conversation',
            created_at: Math.floor(Date.now() / 1000),
            metadata: body.metadata,
          };
        state.objects[id] = value;
        state.items[id] = items(body.items || []);
        state.calls.conversations++;
        save();
        return reply(res, 200, value);
      }
      if (req.method === 'POST' && path === 'responses') {
        if (
          !proof(body) ||
          body.model !== 'state-probe' ||
          (body.store !== true && body.store !== false) ||
          body.background !== false
        )
          throw Error('unbound response');
        if (body.previous_response_id && !state.objects[body.previous_response_id])
          throw Error('unknown previous');
        if (body.conversation && !state.objects[body.conversation])
          throw Error('unknown conversation');
        const id = 'resp_fixture' + ++state.serial,
          item = {
            id: 'msg_fixture' + ++state.serial,
            type: 'message',
            role: 'assistant',
            content: [{ type: 'output_text', text: 'Persistent state fixture accepted' }],
          };
        const value = {
          id,
          object: 'response',
          model: 'state-probe',
          status: 'completed',
          metadata: body.metadata,
          previous_response_id: body.previous_response_id || null,
          conversation: body.conversation ? { id: body.conversation } : null,
          output: [item],
          usage: { input_tokens: 9, output_tokens: 2, total_tokens: 11 },
        };
        if (body.store) {
          state.objects[id] = value;
          state.items[id] = items([
            { type: 'message', role: 'user', content: 'Synthetic stored input' },
          ]);
        }
        if (body.conversation) state.items[body.conversation].push(item);
        state.calls.responses++;
        save();
        if (!body.stream) return reply(res, 200, value);
        res.writeHead(200, { 'Content-Type': 'text/event-stream' });
        const created = { ...value, status: 'in_progress', output: [], usage: null };
        for (const event of [
          { type: 'response.created', response: created },
          { type: 'response.output_item.added', response_id: id, item: { ...item, content: [] } },
          {
            type: 'response.output_text.delta',
            response_id: id,
            item_id: item.id,
            delta: 'Persistent state fixture accepted',
          },
          { type: 'response.completed', response: value },
        ])
          res.write(`event: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`);
        return res.end();
      }
      const id = parts[1],
        value = state.objects[id];
      if (!value) return reply(res, 404, { error: 'unknown fixture resource' });
      if (parts.length >= 3) {
        const data = state.items[id] || [];
        if (parts[3]) {
          const index = data.findIndex((v) => v.id === parts[3]);
          if (index < 0) return reply(res, 404, { error: 'unknown fixture item' });
          if (req.method === 'DELETE') {
            data.splice(index, 1);
            state.calls.deletes++;
            save();
            return reply(res, 200, value);
          }
          state.calls.reads++;
          save();
          return reply(res, 200, data[index]);
        }
        if (req.method === 'POST') {
          const added = items(body.items);
          data.push(...added);
          state.calls.items++;
          save();
          return reply(res, 200, list(added));
        }
        state.calls.reads++;
        save();
        const start = url.searchParams.has('after')
            ? data.findIndex((v) => v.id === url.searchParams.get('after')) + 1
            : 0,
          limit = Number(url.searchParams.get('limit') || 20),
          page = data.slice(start, start + limit);
        return reply(res, 200, { ...list(page), has_more: start + limit < data.length });
      }
      if (req.method === 'DELETE') {
        delete state.objects[id];
        state.calls.deletes++;
        save();
        return reply(res, 200, {
          id,
          deleted: true,
          object: parts[0] === 'responses' ? 'response.deleted' : 'conversation.deleted',
        });
      }
      if (req.method === 'POST') {
        if (!proof(body)) throw Error('lost proof');
        value.metadata = body.metadata;
        state.calls.conversations++;
      } else state.calls.reads++;
      save();
      return reply(res, 200, value);
    } catch {
      return reply(res, 400, { error: 'fixture rejected request' });
    }
  })
  .listen(9081, '0.0.0.0');
