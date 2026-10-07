import assert from 'node:assert/strict';
import { test } from 'node:test';
import {
  followOperation,
  transientOperationRead,
  retryAfterSeconds,
} from '../src/shared/followOperation.ts';

test('observation honors Retry-After within the deadline without retrying mutation', async () => {
  assert.equal(retryAfterSeconds('12'), 12);
  assert.equal(retryAfterSeconds('9999'), 600);
  assert.equal(retryAfterSeconds('bad-date'), undefined);
  assert.equal(retryAfterSeconds('Thu, 01 Jan 1970 00:00:12 GMT', 0), 12);
  let clock = 0;
  let reads = 0;
  const value = await followOperation({
    id: 'op_one',
    signal: new AbortController().signal,
    timeoutMs: 20_000,
    now: () => clock,
    wait: async (ms) => {
      clock += ms;
    },
    read: async () => {
      reads++;
      if (reads === 1) throw { status: 429, retryAfterSeconds: 12 };
      return { id: 'op_one', state: 'succeeded' };
    },
    onValue: () => {},
  });
  assert.equal(clock, 12000);
  assert.equal(value?.state, 'succeeded');
  assert.equal(reads, 2);
});

test('transient observation recovers the same accepted Operation without another mutation', async () => {
  let clock = 0;
  let reads = 0;
  let unavailable = 0;
  const values: string[] = [];
  const delays: number[] = [];
  const result = await followOperation({
    id: 'op_one',
    signal: new AbortController().signal,
    now: () => clock,
    wait: async (ms) => {
      clock += ms;
      delays.push(ms);
    },
    read: async () => {
      reads++;
      if (reads < 3) throw Object.assign(new Error('unavailable'), { status: 503 });
      return { id: 'op_one', state: reads === 3 ? 'running' : 'succeeded' };
    },
    onValue: (v) => values.push(v.state),
    onUnavailable: () => unavailable++,
  });
  assert.equal(result?.id, 'op_one');
  assert.equal(reads, 4);
  assert.equal(unavailable, 2);
  assert.deepEqual(values, ['running', 'succeeded']);
  assert.deepEqual(delays, [2000, 4000, 2000]);
});

test('permission loss and unknown resource stop immediately', async () => {
  for (const status of [401, 403, 404, 409, 422]) {
    let reads = 0;
    await assert.rejects(
      followOperation({
        id: 'op_one',
        signal: new AbortController().signal,
        read: async () => {
          reads++;
          throw Object.assign(new Error('denied'), { status });
        },
        onValue: () => {
          throw Error('Must not observe');
        },
      }),
      /denied/,
    );
    assert.equal(reads, 1);
  }
  for (const status of [429, 500, 502, 503, 504])
    assert.equal(transientOperationRead({ status }), true);
  assert.equal(transientOperationRead(new TypeError('fetch failed')), true);
});

test('observation timeout leaves backend state uncertain and bounded', async () => {
  let clock = 0;
  let reads = 0;
  const result = await followOperation({
    id: 'op_one',
    timeoutMs: 5000,
    signal: new AbortController().signal,
    now: () => clock,
    wait: async (ms) => {
      clock += ms;
    },
    read: async () => {
      reads++;
      throw { status: 502 };
    },
    onValue: () => {
      throw Error('Must not mark failed');
    },
  });
  assert.equal(result, null);
  assert.equal(clock, 5000);
  assert.equal(reads, 2);
});

test('cancelled is terminal; a different identity or malformed state cannot become success', async () => {
  const controller = new AbortController();
  const result = await followOperation({
    id: 'op_one',
    signal: controller.signal,
    read: async () => ({ id: 'op_one', state: 'cancelled' }),
    onValue: () => {},
  });
  assert.equal(result?.state, 'cancelled');
  for (const value of [
    { id: 'op_other', state: 'succeeded' },
    { id: 'op_one', state: 'unknown' },
  ])
    await assert.rejects(
      followOperation({
        id: 'op_one',
        signal: controller.signal,
        read: async () => value,
        onValue: () => {
          throw Error('Wrong resource');
        },
      }),
      /Invalid Operation/,
    );
});

test('unmount aborts an in-flight read; request timeout can recover without cancelling backend', async () => {
  const controller = new AbortController();
  const waiting = followOperation({
    id: 'op_one',
    signal: controller.signal,
    read: (signal) =>
      new Promise<{ id: string; state: string }>((_, reject) => {
        signal.addEventListener('abort', () => reject(signal.reason), { once: true });
      }),
    onValue: () => {},
  });
  controller.abort();
  await assert.rejects(waiting, { name: 'AbortError' });
  let reads = 0;
  const keepAlive = setTimeout(() => {}, 1000);
  try {
    const result = await followOperation({
      id: 'op_one',
      signal: new AbortController().signal,
      requestTimeoutMs: 10,
      wait: async () => {},
      read: (signal) => {
        reads++;
        if (reads === 2) return Promise.resolve({ id: 'op_one', state: 'succeeded' });
        return new Promise<{ id: string; state: string }>((_, reject) =>
          signal.addEventListener('abort', () => reject(signal.reason), { once: true }),
        );
      },
      onValue: () => {},
    });
    assert.equal(result?.state, 'succeeded');
    assert.equal(reads, 2);
  } finally {
    clearTimeout(keepAlive);
  }
});
