import { readFileSync } from 'node:fs';
import { createHash } from 'node:crypto';

const methods = new Set(['get', 'head', 'post', 'put', 'patch', 'delete', 'options']);
export const contract = JSON.parse(readFileSync(new URL('../contracts/openapi-v1.json', import.meta.url), 'utf8'));
export const catalogHash = createHash('sha256').update(JSON.stringify(contract)).digest('hex');

export function resolve(value) {
  const visited = new Set();
  while (value?.$ref) {
    const ref = value.$ref;
    if (!ref.startsWith('#/') || visited.has(ref)) throw new Error('Invalid local contract reference');
    visited.add(ref);
    const target = ref.slice(2).split('/').reduce((v, key) => v?.[key.replaceAll('~1', '/').replaceAll('~0', '~')], contract);
    if (!target) throw new Error('Missing local contract reference');
    const { $ref, ...siblings } = value;
    value = { ...target, ...siblings };
  }
  return value;
}

const byId = new Map();
for (const [path, item] of Object.entries(contract.paths)) {
  for (const [method, operation] of Object.entries(item)) {
    if (!methods.has(method)) continue;
    const id = operation.operationId;
    if (!id || byId.has(id) || !path.startsWith('/') || path.startsWith('//')) throw new Error('Invalid operation catalog');
    const parameters = new Map();
    for (const entry of [...(item.parameters || []), ...(operation.parameters || [])]) {
      const parameter = resolve(entry);
      parameters.set(parameter.in + ':' + parameter.name, parameter);
    }
    byId.set(id, Object.freeze({
      id, path, method: method.toUpperCase(), read_only: ['get', 'head', 'options'].includes(method),
      summary: operation.summary || id, description: operation.description || '', tags: operation.tags || [],
      parameters: [...parameters.values()], request_body: operation.requestBody ? resolve(operation.requestBody) : null,
      responses: operation.responses || {}, security: operation.security || contract.security || [],
    }));
  }
}
export const operations = Object.freeze([...byId.values()].sort((a, b) => a.id < b.id ? -1 : a.id > b.id ? 1 : 0));

export function describe(id) {
  const value = byId.get(id);
  if (!value) throw new Error('Unknown operation_id; search the bundled contract first');
  return value;
}

export function operationDetail(id) {
  const operation = describe(id), references = {};
  function collect(value) {
    if (!value || typeof value !== 'object') return;
    if (value.$ref && !Object.hasOwn(references, value.$ref)) {
      references[value.$ref] = resolve({ $ref: value.$ref });
      collect(references[value.$ref]);
    }
    for (const item of Object.values(value)) collect(item);
  }
  collect(operation);
  return { api_version: contract.info.version, catalog_sha256: catalogHash, operation, referenced_schemas: references };
}

export function search({ query = '', method = '', tag = '', cursor, limit = 25 } = {}) {
  if (typeof query !== 'string' || query.length > 200 || typeof method !== 'string' || typeof tag !== 'string' || tag.length > 80 || !Number.isInteger(limit) || limit < 1 || limit > 50)
    throw new Error('Invalid catalog search');
  const filters = { query: query.trim().toLowerCase(), method: method.toUpperCase(), tag };
  if (filters.method && !methods.has(filters.method.toLowerCase())) throw new Error('Unknown HTTP method');
  const signature = createHash('sha256').update(JSON.stringify(filters)).digest('hex').slice(0, 16);
  let offset = 0;
  if (cursor !== undefined) {
    if (typeof cursor !== 'string' || cursor.length > 256 || !/^[A-Za-z0-9_-]+$/.test(cursor)) throw new Error('Invalid catalog cursor');
    let decoded;
    try { decoded = JSON.parse(Buffer.from(cursor, 'base64url').toString('utf8')); } catch { throw new Error('Invalid catalog cursor'); }
    if (decoded.catalog !== catalogHash || decoded.filters !== signature || !Number.isInteger(decoded.offset) || decoded.offset < 0) throw new Error('Stale or mismatched catalog cursor');
    offset = decoded.offset;
  }
  const items = operations.filter(op => (!filters.method || op.method === filters.method) && (!tag || op.tags.includes(tag)) &&
    (!filters.query || [op.id, op.path, op.summary, op.description, ...op.tags].join(' ').toLowerCase().includes(filters.query)));
  if (offset > items.length) throw new Error('Invalid catalog cursor');
  const next = offset + limit;
  return { api_version: contract.info.version, catalog_sha256: catalogHash, total: items.length,
    items: items.slice(offset, next).map(({ id, method, path, summary, read_only, tags }) => ({ operation_id: id, method, path, summary, read_only, tags })),
    next_cursor: next < items.length ? Buffer.from(JSON.stringify({ catalog: catalogHash, filters: signature, offset: next })).toString('base64url') : null };
}
