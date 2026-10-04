import assert from 'node:assert/strict';
import { webcrypto } from 'node:crypto';
import { test } from 'node:test';
import { newRequestKey } from '../src/shared/requestKey.ts';

test('remote HTTP creation works when randomUUID is unavailable', () => {
  const descriptor = Object.getOwnPropertyDescriptor(globalThis, 'crypto');
  const random = Math.random;
  try {
    Object.defineProperty(globalThis, 'crypto', {
      configurable: true,
      value: { getRandomValues: webcrypto.getRandomValues.bind(webcrypto) },
    });
    Math.random = () => {
      throw new Error('Insecure random source used');
    };
    const keys = new Set(Array.from({ length: 256 }, newRequestKey));
    assert.equal(keys.size, 256);
    for (const key of keys)
      assert.match(key, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
  } finally {
    Object.defineProperty(globalThis, 'crypto', descriptor!);
    Math.random = random;
  }
});

test('missing Web Crypto fails explicitly without insecure fallback', () => {
  const descriptor = Object.getOwnPropertyDescriptor(globalThis, 'crypto');
  try {
    Object.defineProperty(globalThis, 'crypto', { configurable: true, value: undefined });
    assert.throws(newRequestKey, /Web Crypto/);
  } finally {
    Object.defineProperty(globalThis, 'crypto', descriptor!);
  }
});
