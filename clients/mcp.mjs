#!/usr/bin/env node
import { McpServer } from '@modelcontextprotocol/server';
import { serveStdio, StdioServerTransport } from '@modelcontextprotocol/server/stdio';
import { z } from 'zod';
import { pathToFileURL } from 'node:url';
import { search, operationDetail, describe } from './catalog.mjs';
import { ControlClient, loadProfile } from './client.mjs';

const values = z.record(z.string().max(100), z.unknown());
const invocation = z.object({
  operation_id: z.string().min(1).max(200), path: z.record(z.string(), z.string()).optional(),
  query: values.optional(), headers: z.record(z.string(), z.string()).optional(), body: z.unknown().optional(),
  content_type: z.string().max(100).optional(),
}).strict();
const annotations = readOnly => ({ readOnlyHint: readOnly, destructiveHint: !readOnly, idempotentHint: readOnly, openWorldHint: true });
const result = value => ({ content: [{ type: 'text', text: JSON.stringify(value) }], structuredContent: value });

export function createServer(profile, environment = process.env) {
  const client = new ControlClient(profile, environment);
  const server = new McpServer({ name: 'neon-self-hosted-control', version: '0.1.0' }, {
    instructions: 'Use api_search then api_describe. Requests use the operator-configured origin and credential. Follow server permission, current versions and explicit idempotency keys. An interrupted mutation is not permission to generate a new key or create a replacement resource. Results redact credentials; use the CLI private output file for one-time secrets.',
  });
  server.server.onclose = () => client.close();
  server.registerTool('api_search', {
    description: 'Search the bundled self-hosted OpenAPI operation catalog; no network request.',
    inputSchema: z.object({ query: z.string().max(200).optional(), method: z.string().max(10).optional(), tag: z.string().max(80).optional(), cursor: z.string().max(256).optional(), limit: z.number().int().min(1).max(50).optional() }).strict(),
    annotations: { ...annotations(true), openWorldHint: false },
  }, input => result(search(input)));
  server.registerTool('api_describe', {
    description: 'Read the exact operation method, parameters, body schema, security and response contract before an invocation.',
    inputSchema: z.object({ operation_id: z.string().min(1).max(200) }).strict(),
    annotations: { ...annotations(true), openWorldHint: false },
  }, ({ operation_id }) => result(operationDetail(operation_id)));
  async function invoke(input, context, write) {
    try {
      const { operation_id, ...request } = input;
      const op = describe(operation_id);
      if (op.read_only === write) throw new Error(write ? 'Use api_read for a read-only operation' : 'Use the separately approved api_mutate tool for writes');
      const value = await client.execute(operation_id, request, { allowWrite: write, signal: context.mcpReq.signal });
      return { ...result(value), ...(value.status >= 400 ? { isError: true } : {}) };
    } catch (error) {
      const message = error instanceof Error ? error.message : 'Control API invocation failed';
      return { ...result({ error: message, automatic_retry_sent: false }), isError: true };
    }
  }
  server.registerTool('api_read', {
    description: 'Invoke a GET/HEAD/OPTIONS from the fixed contract using the configured credential; server roles and scopes remain authoritative. Bounded response; no automatic retries.',
    inputSchema: invocation, annotations: annotations(true),
  }, (input, context) => invoke(input, context, false));
  if (profile.allow_mutations) server.registerTool('api_mutate', {
    description: 'Explicitly approved write invocation. May create/delete resources or spend configured model credits. Supply the exact operation body, required versions and caller-owned original idempotency key. Never resend an uncertain mutation with a new key.',
    inputSchema: invocation, annotations: annotations(false),
  }, (input, context) => invoke(input, context, true));
  return server;
}

export function main() {
  const profile = loadProfile();
  const handle = serveStdio(() => createServer(profile), {
    transport: new StdioServerTransport(process.stdin, process.stdout, { maxBufferSize: 2 * 1024 * 1024 }),
    legacy: 'serve', maxSubscriptions: 0,
    onerror: () => process.stderr.write('MCP transport rejected an invalid or closed connection\n'),
  });
  for (const signal of ['SIGINT','SIGTERM']) process.once(signal, () => { void handle.close(); });
  return handle;
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try { main(); } catch { process.stderr.write('Control MCP startup failed; inspect the protected profile and pinned dependencies\n'); process.exitCode = 1; }
}
