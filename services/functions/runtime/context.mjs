import { AsyncLocalStorage } from 'node:async_hooks';

export const invocationContext = new AsyncLocalStorage();

// This self-hosted SDK has an explicit namespace. It is not a claim of wire or
// package compatibility with Neon's proprietary Functions runtime.
export function waitUntil(work) {
  const current = invocationContext.getStore();
  if (!current || current.closed) throw new Error('waitUntil requires an active invocation');
  if (current.pending.length >= 64) throw new Error('waitUntil limit exceeded');
  // Attach a rejection handler immediately, even if work rejects before the
  // response finishes. Outcomes are counted without exposing error messages.
  current.pending.push(Promise.resolve(work).then(() => true, () => false));
}
