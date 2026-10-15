import assert from 'node:assert/strict';
import { test } from 'node:test';
import { ApiError, decodeAPIResponse } from '../src/shared/apiResponse.ts';
import { followOperation } from '../src/shared/followOperation.ts';

test('204 deletion receipts have no JSON representation', async () => {
  assert.equal(await decodeAPIResponse<void>(new Response(null, { status: 204 })), undefined);
});

test('truncated JSON and HTML cannot become successful empty API objects', async () => {
  for (const body of ['{"id":', '<html>upstream error</html>', 'null', '[]', 'true']) {
    await assert.rejects(
      decodeAPIResponse(new Response(body)),
      (error: unknown) =>
        error instanceof ApiError && error.status === 502 && error.code === 'invalid_api_response',
    );
  }
});

test('read observer recovers malformed response for the same accepted identity', async () => {
  let reads = 0;
  let clock = 0;
  const states: string[] = [];
  const result = await followOperation({
    id: 'op_original',
    signal: new AbortController().signal,
    now: () => clock,
    wait: async (ms) => {
      clock += ms;
    },
    read: async () => {
      reads++;
      return decodeAPIResponse<{ id: string; state: string }>(
        new Response(
          reads < 3
            ? reads === 1
              ? '{"id":'
              : '<html>unexpected intermediary</html>'
            : '{"id":"op_original","state":"succeeded"}',
        ),
      );
    },
    onValue: (value) => {
      states.push(value.state);
    },
  });
  assert.equal(result?.id, 'op_original');
  assert.equal(reads, 3);
  assert.equal(clock, 6000);
  assert.deepEqual(states, ['succeeded']);
});

test('HTTP permission errors and Retry-After retain their original semantics', async () => {
  for (const body of [
    '<html>denied</html>',
    '{"code":"denied","message":"No access","request_id":"request-one"}',
  ]) {
    await assert.rejects(
      decodeAPIResponse(new Response(body, { status: 403 })),
      (error: unknown) => error instanceof ApiError && error.status === 403,
    );
  }
  await assert.rejects(
    decodeAPIResponse(new Response('broken', { status: 503, headers: { 'Retry-After': '12' } })),
    (error: unknown) =>
      error instanceof ApiError && error.status === 503 && error.retryAfterSeconds === 12,
  );
});

test('well formed wrong operation identity still fails without accepting success', async () => {
  await assert.rejects(
    followOperation({
      id: 'op_original',
      signal: new AbortController().signal,
      read: () =>
        decodeAPIResponse<{ id: string; state: string }>(
          new Response('{"id":"op_other","state":"succeeded"}'),
        ),
      onValue: () => {
        throw new Error('Must not accept foreign operation');
      },
    }),
    /Invalid Operation response/,
  );
});
