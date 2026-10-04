/**
 * Keep creation replays stable, using only the browser CSPRNG.
 * randomUUID requires a secure context; getRandomValues also supports the
 * explicitly permitted HTTP laboratory console. Production still uses HTTPS.
 */
export function newRequestKey(): string {
  const crypto = globalThis.crypto;
  if (!crypto?.getRandomValues) {
    throw new Error('浏览器无法生成安全请求标识，请使用支持 Web Crypto 的现代浏览器。');
  }
  if (typeof crypto.randomUUID === 'function') return crypto.randomUUID();
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes, (value) => value.toString(16).padStart(2, '0')).join('');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}
