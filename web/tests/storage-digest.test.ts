import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { test } from 'node:test';
import { storageBlobDigest } from '../src/features/storage/fileDigest.ts';

test('large file digest uses bounded slices and matches Node SHA-256', async () => {
  const bytes = new Uint8Array((17 << 20) + 9).fill(120);
  class BoundedBlob extends Blob {
    override async arrayBuffer(): Promise<ArrayBuffer> {
      throw Error('Whole-file allocation attempted');
    }
    override slice(start = 0, end = this.size, type?: string) {
      assert(end - start <= 1024 * 1024);
      return super.slice(start, end, type);
    }
  }
  const actual = await storageBlobDigest(new BoundedBlob([bytes]));
  const expected = createHash('sha256').update(bytes).digest();
  assert.equal(actual.hex, expected.toString('hex'));
  assert.equal(actual.base64, expected.toString('base64'));
});

test('empty object has its exact full-object checksum', async () => {
  const actual = await storageBlobDigest(new Blob([]));
  assert.equal(actual.hex, createHash('sha256').digest('hex'));
  assert.equal(actual.base64, createHash('sha256').digest('base64'));
});

test('paused multipart hashing stops before reading another file slice', async () => {
  const controller = new AbortController();
  let reads = 0;
  class CancelledBlob extends Blob {
    override slice(start = 0, end = this.size, type?: string) {
      const part = super.slice(start, end, type);
      const read = part.arrayBuffer.bind(part);
      part.arrayBuffer = async () => {
        reads++;
        const result = await read();
        controller.abort(Error('original upload paused'));
        return result;
      };
      return part;
    }
  }
  await assert.rejects(
    storageBlobDigest(new CancelledBlob([new Uint8Array(3 << 20)]), controller.signal),
    /original upload paused/,
  );
  assert.equal(reads, 1);
});
