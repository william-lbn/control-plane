// Shared bounded decoder for explicitly authorized Console AI requests. Invalid
// UTF-8, oversized or incomplete representations never become usable drafts.
export async function responseJSON(response: Response) {
  const reader = response.body?.getReader();
  if (!reader) throw new Error('模型响应体不可用。');
  const decoder = new TextDecoder('utf-8', { fatal: true });
  let bytes = 0,
    text = '';
  try {
    while (true) {
      const part = await reader.read();
      if (part.done) break;
      bytes += part.value.length;
      if (bytes > 65536) throw new Error('模型响应超过界限，不能载入。');
      text += decoder.decode(part.value, { stream: true });
    }
    text += decoder.decode();
    return JSON.parse(text) as unknown;
  } finally {
    await reader.cancel().catch(() => {});
  }
}

export function consoleCSRF() {
  const raw =
    document.cookie
      .split(';')
      .map((v) => v.trim())
      .find((v) => v.startsWith('neon_v2_csrf='))
      ?.slice('neon_v2_csrf='.length) || '';
  return decodeURIComponent(raw);
}
