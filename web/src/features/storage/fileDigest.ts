import { sha256 } from '@noble/hashes/sha2.js';
import { bytesToHex } from '@noble/hashes/utils.js';

// Work on bounded slices so preparing a capability or resuming multipart does
// not retain the entire file in JavaScript memory. File/Blob sends stream later.
export async function storageBlobDigest(blob: Blob, signal?: AbortSignal) {
  const hash = sha256.create();
  const block = 1024 * 1024;
  for (let offset = 0; offset < blob.size; offset += block) {
    signal?.throwIfAborted();
    const bytes = new Uint8Array(await blob.slice(offset, offset + block).arrayBuffer());
    signal?.throwIfAborted();
    hash.update(bytes);
  }
  signal?.throwIfAborted();
  const bytes = hash.digest();
  return { hex: bytesToHex(bytes), base64: btoa(String.fromCharCode(...bytes)) };
}
